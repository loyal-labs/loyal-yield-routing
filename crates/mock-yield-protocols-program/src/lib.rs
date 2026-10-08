#![allow(unexpected_cfgs)]

use solana_program::{
    account_info::{next_account_info, AccountInfo},
    entrypoint,
    entrypoint::ProgramResult,
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    pubkey,
    pubkey::Pubkey,
    sysvar::Sysvar,
};
use spl_token::solana_program::program_pack::Pack;

pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const LOYAL_HUB_SWAP_PROGRAM_ID: Pubkey =
    pubkey!("LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH");
pub const USDC_MINT: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const PYUSD_MINT: Pubkey = pubkey!("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
pub const WRAPPED_SOL_MINT: Pubkey = pubkey!("So11111111111111111111111111111111111111112");
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD");
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
pub const USDC_DECIMALS: u8 = 6;
pub const PYUSD_DECIMALS: u8 = 6;
pub const KAMINO_COLLATERAL_DECIMALS: u8 = 6;
pub const JUPITER_ROUTER_USDC_PYUSD_DISCRIMINATOR: [u8; 8] = [187, 100, 250, 204, 49, 196, 175, 20];
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [216, 224, 191, 27, 204, 151, 102, 175];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];
pub const JUPITER_SWAP_AUTHORITY_SEED: &[u8] = b"jupiter-swap-authority";
pub const KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED: &[u8] = b"kamino-reserve-liq-authority";
pub const KAMINO_COLLATERAL_MINT_AUTHORITY_SEED: &[u8] = b"kamino-collateral-mint-authority";
pub const KAMINO_LENDING_MARKET_AUTHORITY_SEED: &[u8] = b"lma";
pub const KAMINO_RESERVE_STATE_LEN: usize = 160;
const KAMINO_RESERVE_DISCRIMINATOR: [u8; 8] = [43, 242, 204, 202, 26, 247, 59, 127];
const KAMINO_OBLIGATION_DISCRIMINATOR: [u8; 8] = [168, 206, 141, 106, 88, 76, 172, 167];
const KAMINO_RESERVE_LENDING_MARKET_OFFSET: usize = 32;
const KAMINO_RESERVE_LIQUIDITY_MINT_OFFSET: usize = 128;
const KAMINO_RESERVE_LIQUIDITY_SUPPLY_OFFSET: usize = 160;
const KAMINO_RESERVE_COLLATERAL_MINT_OFFSET: usize = 2560;
const KAMINO_RESERVE_COLLATERAL_SUPPLY_OFFSET: usize = 2600;
const KAMINO_OBLIGATION_FIRST_DEPOSIT_RESERVE_OFFSET: usize = 96;
const KAMINO_OBLIGATION_FIRST_DEPOSIT_AMOUNT_OFFSET: usize = 128;

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
    Borrow { amount: u64 },
    Repay { amount: u64 },
}

pub fn process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    if program_id == &JUPITER_V6_PROGRAM_ID {
        if data.starts_with(&MULTIPLY_SHARED_ROUTE_DISCRIMINATOR) {
            return process_multiply_shared_route(program_id, accounts, data);
        }
        if accounts.len() == 24
            && data.len() == 40
            && data[..8] == JUPITER_ROUTER_USDC_PYUSD_DISCRIMINATOR
        {
            return process_local_alphaq_route(program_id, accounts, data);
        }
        return process_jupiter(program_id, accounts, data);
    }

    if program_id == &KAMINO_LEND_PROGRAM_ID {
        return process_kamino(program_id, accounts, data);
    }

    if program_id == &LOYAL_HUB_SWAP_PROGRAM_ID {
        return process_adversarial_loyal_hub_swap(program_id, accounts, data);
    }

    Err(ProgramError::IncorrectProgramId)
}

fn process_adversarial_loyal_hub_swap(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    if data.len() != loyal_hub_abi::SWAP_EXACT_IN_DATA_LEN
        || data[loyal_hub_abi::SWAP_EXACT_IN_TAG_OFFSET as usize] != loyal_hub_abi::SWAP_EXACT_IN
    {
        return Err(ProgramError::InvalidInstructionData);
    }

    let amount_in = read_u64_at(data, loyal_hub_abi::SWAP_EXACT_IN_AMOUNT_IN_DATA_OFFSET)?;
    let amount_out = read_u64_at(data, loyal_hub_abi::SWAP_EXACT_IN_AMOUNT_OUT_DATA_OFFSET)?;
    let lane_id = data[loyal_hub_abi::SWAP_EXACT_IN_LANE_ID_DATA_OFFSET as usize];

    let account_info_iter = &mut accounts.iter();
    let _config = next_account_info(account_info_iter)?;
    let user_vault = next_account_info(account_info_iter)?;
    let user_input = next_account_info(account_info_iter)?;
    let user_output = next_account_info(account_info_iter)?;
    let hub_input = next_account_info(account_info_iter)?;
    let hub_output = next_account_info(account_info_iter)?;
    let input_mint = next_account_info(account_info_iter)?;
    let output_mint = next_account_info(account_info_iter)?;
    let hub_authority = next_account_info(account_info_iter)?;
    let _hub_authorizer = next_account_info(account_info_iter)?;
    let token_program = next_account_info(account_info_iter)?;

    require_signer(user_vault)?;
    require_key(token_program, &spl_token::id())?;

    let input_decimals = spl_token::state::Mint::unpack(&input_mint.data.borrow())?.decimals;
    let output_decimals = spl_token::state::Mint::unpack(&output_mint.data.borrow())?.decimals;

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
        &[loyal_hub_abi::HUB_AUTHORITY_SEED, &[lane_id]],
    )
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

// Narrow executable model of the verifier's one-hop RouteV2 envelope. This
// executes SPL transfers, not the production AlphaQ pricing/CPI implementation.
// The fixture's pool inventory is owned by Jupiter's event-authority PDA because
// that PDA is present in the unchanged official RouteV2 account layout.
fn process_local_alphaq_route(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    if accounts.len() != 24
        || data[26] != 0
        || data[27..] != [0, 0, 0, 1, 0, 0, 0, 104, 1, 0x10, 0x27, 0, 1]
    {
        return Err(ProgramError::InvalidInstructionData);
    }
    require_signer(&accounts[0])?;
    require_key(&accounts[5], &spl_token::id())?;
    require_key(&accounts[6], &spl_token::id())?;
    require_key(&accounts[7], program_id)?;
    require_key(&accounts[9], program_id)?;
    require_key(&accounts[14], accounts[1].key)?;
    require_key(&accounts[15], accounts[2].key)?;
    let authority = Pubkey::find_program_address(&[b"__event_authority"], program_id).0;
    require_key(&accounts[8], &authority)?;
    let input = read_u64(&data[8..16])?;
    let output = read_u64(&data[16..24])?;
    if input == 0 || output == 0 || accounts[3].key == accounts[4].key {
        return Err(ProgramError::InvalidInstructionData);
    }
    let input_decimals = spl_token::state::Mint::unpack(&accounts[3].data.borrow())?.decimals;
    let output_decimals = spl_token::state::Mint::unpack(&accounts[4].data.borrow())?.decimals;
    transfer_checked(
        &accounts[1],
        &accounts[3],
        &accounts[16],
        &accounts[0],
        &accounts[5],
        input,
        input_decimals,
    )?;
    transfer_checked_signed(
        program_id,
        &accounts[17],
        &accounts[4],
        &accounts[2],
        &accounts[8],
        &accounts[6],
        output,
        output_decimals,
        &[b"__event_authority"],
    )
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
    // The local fixed-price model does not accrue interest or consult oracles.
    // Accept only the official refresh wire shape and validated fixture accounts;
    // do not blanket-accept unknown public instructions ahead of asset movement.
    if data == [2, 218, 138, 235, 79, 201, 25, 102] {
        if accounts.len() != 6
            || accounts[0].owner != program_id
            || !accounts[0].is_writable
            || accounts[1].owner != program_id
            || accounts[2..5].iter().any(|account| {
                account.key != program_id || account.is_writable || account.is_signer
            })
            || !fixture_scope_refresh_account(&accounts[5], program_id)
        {
            return Err(ProgramError::InvalidAccountData);
        }
        let reserve = read_kamino_reserve_state(&accounts[0])?;
        return require_key(&accounts[1], &reserve.lending_market);
    }
    if data == [33, 132, 147, 228, 151, 192, 72, 89] {
        if accounts.len() < 2
            || accounts[0].owner != program_id
            || accounts[1].owner != program_id
            || !accounts[1].is_writable
        {
            return Err(ProgramError::InvalidAccountData);
        }
        let obligation = accounts[1].try_borrow_data()?;
        if obligation.len() < 3344
            || !obligation.starts_with(&KAMINO_OBLIGATION_DISCRIMINATOR)
            || obligation[32..64] != accounts[0].key.to_bytes()
        {
            return Err(ProgramError::InvalidAccountData);
        }
        for account in &accounts[2..] {
            if account.owner != program_id {
                return Err(ProgramError::IllegalOwner);
            }
            let reserve = read_kamino_reserve_state(account)?;
            require_key(&accounts[0], &reserve.lending_market)?;
        }
        return Ok(());
    }
    if data == [251, 10, 231, 76, 27, 11, 159, 96, 0, 0] {
        return process_kamino_init_obligation(program_id, accounts);
    }
    match parse_kamino_instruction(data)? {
        KaminoInstruction::Deposit { amount } => {
            process_kamino_deposit(program_id, accounts, amount)
        }
        KaminoInstruction::Withdraw { amount } => {
            process_kamino_withdraw(program_id, accounts, amount)
        }
        KaminoInstruction::Borrow { amount } => {
            process_multiply_debt(program_id, accounts, amount, true)
        }
        KaminoInstruction::Repay { amount } => {
            process_multiply_debt(program_id, accounts, amount, false)
        }
    }
}

// init_obligation (tag 0, id 0) in the official account order: the vanilla
// obligation PDA is created through the System program, rent paid by the
// signing fee payer, and stamped with its market and owner. User metadata and
// referrer state are not modeled.
fn process_kamino_init_obligation(program_id: &Pubkey, accounts: &[AccountInfo]) -> ProgramResult {
    let [owner, fee_payer, obligation, market, seed1, seed2, _metadata, _rent, system] = accounts
    else {
        return Err(ProgramError::NotEnoughAccountKeys);
    };
    let seeds: [&[u8]; 6] = [
        &[0],
        &[0],
        owner.key.as_ref(),
        market.key.as_ref(),
        seed1.key.as_ref(),
        seed2.key.as_ref(),
    ];
    let (expected, bump) = Pubkey::find_program_address(&seeds, program_id);
    if !owner.is_signer
        || !fee_payer.is_signer
        || !fee_payer.is_writable
        || !obligation.is_writable
        || obligation.key != &expected
        || obligation.lamports() != 0
        || market.owner != program_id
        || system.key != &solana_program::system_program::ID
    {
        return Err(ProgramError::InvalidAccountData);
    }
    let space = 3344;
    let rent = solana_program::rent::Rent::get()?.minimum_balance(space);
    invoke_signed(
        &solana_program::system_instruction::create_account(
            fee_payer.key,
            obligation.key,
            rent,
            space as u64,
            program_id,
        ),
        &[fee_payer.clone(), obligation.clone(), system.clone()],
        &[&[
            &[0],
            &[0],
            owner.key.as_ref(),
            market.key.as_ref(),
            seed1.key.as_ref(),
            seed2.key.as_ref(),
            &[bump],
        ]],
    )?;
    let mut data = obligation.try_borrow_mut_data()?;
    data[..8].copy_from_slice(&KAMINO_OBLIGATION_DISCRIMINATOR);
    data[32..64].copy_from_slice(market.key.as_ref());
    data[64..96].copy_from_slice(owner.key.as_ref());
    Ok(())
}

// The Multiply fixture uses the source-pinned Scope address. This local model
// does not evaluate prices or pretend to model mature Scope/KLend risk logic.
const MULTIPLY_FIXTURE_SCOPE: Pubkey = pubkey!("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH");
fn fixture_scope_refresh_account(account: &AccountInfo, program_id: &Pubkey) -> bool {
    !account.is_writable
        && !account.is_signer
        && (account.key == program_id || account.key == &MULTIPLY_FIXTURE_SCOPE)
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
    // SHA256 global names and ordered accounts verified against locked
    // KLend SDK 23b9f2b instructions/{borrow,repay}.rs by the Rust producer.
    if data[..8] == [161, 128, 143, 245, 171, 199, 194, 6] {
        return Ok(KaminoInstruction::Borrow { amount });
    }
    if data[..8] == [116, 174, 213, 76, 180, 53, 210, 144] {
        return Ok(KaminoInstruction::Repay { amount });
    }

    Err(ProgramError::InvalidInstructionData)
}

struct KaminoDepositAccounts<'a, 'info> {
    owner: &'a AccountInfo<'info>,
    obligation: &'a AccountInfo<'info>,
    reserve: &'a AccountInfo<'info>,
    lending_market: &'a AccountInfo<'info>,
    lending_market_authority: &'a AccountInfo<'info>,
    reserve_liquidity_mint: &'a AccountInfo<'info>,
    reserve_liquidity_supply: &'a AccountInfo<'info>,
    reserve_collateral_mint: &'a AccountInfo<'info>,
    user_source_liquidity: &'a AccountInfo<'info>,
    reserve_destination_deposit_collateral: &'a AccountInfo<'info>,
    collateral_token_program: &'a AccountInfo<'info>,
    liquidity_token_program: &'a AccountInfo<'info>,
    liquidity_decimals: u8,
}

struct KaminoRedeemAccounts<'a, 'info> {
    owner: &'a AccountInfo<'info>,
    obligation: &'a AccountInfo<'info>,
    lending_market: &'a AccountInfo<'info>,
    reserve: &'a AccountInfo<'info>,
    lending_market_authority: &'a AccountInfo<'info>,
    reserve_liquidity_mint: &'a AccountInfo<'info>,
    reserve_collateral_mint: &'a AccountInfo<'info>,
    reserve_liquidity_supply: &'a AccountInfo<'info>,
    reserve_source_collateral: &'a AccountInfo<'info>,
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
    collateral_supply: Pubkey,
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
        kamino.reserve_destination_deposit_collateral,
        kamino.lending_market_authority,
        kamino.collateral_token_program,
        amount,
        KAMINO_COLLATERAL_DECIMALS,
        &[
            KAMINO_LENDING_MARKET_AUTHORITY_SEED,
            kamino.lending_market.key.as_ref(),
        ],
    )?;
    adjust_kamino_obligation(kamino.obligation, kamino.reserve.key, amount, true)
}

fn process_kamino_withdraw(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    amount: u64,
) -> ProgramResult {
    let kamino = parse_kamino_redeem_accounts(program_id, accounts)?;
    let amount = multiply_fixture_withdraw_amount(kamino.obligation, kamino.reserve, amount)?;
    let collateral_owner =
        spl_token::state::Account::unpack(&kamino.reserve_source_collateral.data.borrow())?.owner;
    if collateral_owner == *kamino.owner.key {
        burn_checked(
            kamino.reserve_source_collateral,
            kamino.reserve_collateral_mint,
            kamino.owner,
            kamino.collateral_token_program,
            amount,
            KAMINO_COLLATERAL_DECIMALS,
        )?;
    } else {
        burn_checked_signed(
            program_id,
            kamino.reserve_source_collateral,
            kamino.reserve_collateral_mint,
            kamino.lending_market_authority,
            kamino.collateral_token_program,
            amount,
            KAMINO_COLLATERAL_DECIMALS,
            &[
                KAMINO_LENDING_MARKET_AUTHORITY_SEED,
                kamino.lending_market.key.as_ref(),
            ],
        )?;
    }
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
    )?;
    adjust_kamino_obligation(kamino.obligation, kamino.reserve.key, amount, false)
}

// Narrow fixed-price, zero-interest/fee Multiply bank model. It executes SPL
// movements and checked source-layout liability writes; it does not implement
// mature KLend pricing, accrual, elevation groups, farms, or liquidation.
const MULTIPLY_MARKET: Pubkey = pubkey!("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y");
const MULTIPLY_USDC_RESERVE: Pubkey = pubkey!("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo");
const MULTIPLY_SYRUP_MINT: Pubkey = pubkey!("AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj");
const MULTIPLY_DEBT_FARM: Pubkey = pubkey!("87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y");
const MULTIPLY_FARMS: Pubkey = pubkey!("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr");
const MULTIPLY_SHARED_ROUTE_DISCRIMINATOR: [u8; 8] = [193, 32, 155, 51, 65, 214, 156, 129];

fn multiply_debt_raw(obligation: &AccountInfo) -> Result<u64, ProgramError> {
    let data = obligation.try_borrow_data()?;
    if obligation.owner != &KAMINO_LEND_PROGRAM_ID
        || data.len() != 3344
        || !data.starts_with(&KAMINO_OBLIGATION_DISCRIMINATOR)
    {
        return Err(ProgramError::InvalidAccountData);
    }
    let sf = u128::from_le_bytes(data[1296..1312].try_into().unwrap());
    if sf & ((1u128 << 60) - 1) != 0 {
        return Err(ProgramError::InvalidAccountData);
    }
    u64::try_from(sf >> 60).map_err(|_| ProgramError::ArithmeticOverflow)
}

fn process_multiply_debt(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    requested: u64,
    borrow: bool,
) -> ProgramResult {
    if accounts.len() != if borrow { 15 } else { 13 } || requested == 0 {
        return Err(ProgramError::InvalidInstructionData);
    }
    let (reserve_i, mint_i, supply_i, custody_i, token_i, sysvar_i, farm_i, authority_i) = if borrow
    {
        (4, 5, 6, 8, 10, 11, 12, 3)
    } else {
        (3, 4, 5, 6, 7, 8, 9, 11)
    };
    let owner = &accounts[0];
    let obligation = &accounts[1];
    let market = &accounts[2];
    let reserve = &accounts[reserve_i];
    let mint = &accounts[mint_i];
    let supply = &accounts[supply_i];
    let custody = &accounts[custody_i];
    let token = &accounts[token_i];
    let authority = &accounts[authority_i];
    require_common_kamino_accounts(program_id, owner, market, authority)?;
    require_key(market, &MULTIPLY_MARKET)?;
    require_key(reserve, &MULTIPLY_USDC_RESERVE)?;
    require_key(mint, &USDC_MINT)?;
    require_key(token, &spl_token::id())?;
    require_key(
        &accounts[sysvar_i],
        &solana_program::sysvar::instructions::id(),
    )?;
    require_key(&accounts[farm_i + 1], &MULTIPLY_DEBT_FARM)?;
    require_key(accounts.last().unwrap(), &MULTIPLY_FARMS)?;
    let farm_user = Pubkey::find_program_address(
        &[
            b"user",
            MULTIPLY_DEBT_FARM.as_ref(),
            obligation.key.as_ref(),
        ],
        &MULTIPLY_FARMS,
    )
    .0;
    require_key(&accounts[farm_i], &farm_user)?;
    if accounts[farm_i].owner != &MULTIPLY_FARMS
        || accounts[farm_i + 1].owner != &MULTIPLY_FARMS
        || !accounts[farm_i].is_writable
        || !accounts[farm_i + 1].is_writable
    {
        return Err(ProgramError::InvalidAccountData);
    }
    if reserve.owner != program_id
        || market.owner != program_id
        || !reserve.is_writable
        || !obligation.is_writable
        || !supply.is_writable
        || !custody.is_writable
        || accounts[1..].iter().any(|a| a.is_signer)
    {
        return Err(ProgramError::InvalidAccountData);
    }
    let state = read_kamino_reserve_state(reserve)?;
    require_key(market, &state.lending_market)?;
    require_key(mint, &state.liquidity_mint)?;
    require_key(supply, &state.liquidity_supply)?;
    let data = obligation.try_borrow_data()?;
    if data.len() != 3344
        || data[32..64] != market.key.to_bytes()
        || data[64..96] != owner.key.to_bytes()
        || (data[1208..1240] != [0; 32] && data[1208..1240] != reserve.key.to_bytes())
    {
        return Err(ProgramError::InvalidAccountData);
    }
    let collateral = read_u64(&data[128..136])?;
    drop(data);
    let old_debt = multiply_debt_raw(obligation)?;
    let amount = if borrow {
        requested
    } else {
        requested.min(old_debt)
    };
    if amount == 0 {
        return Err(ProgramError::InvalidInstructionData);
    }
    let source = spl_token::state::Account::unpack(&supply.try_borrow_data()?)?;
    let destination = spl_token::state::Account::unpack(&custody.try_borrow_data()?)?;
    if source.mint != USDC_MINT
        || source.owner != *authority.key
        || destination.mint != USDC_MINT
        || destination.owner != *owner.key
        || supply.owner != token.key
        || custody.owner != token.key
        || mint.owner != token.key
    {
        return Err(ProgramError::InvalidAccountData);
    }
    let next = if borrow {
        old_debt
            .checked_add(amount)
            .ok_or(ProgramError::ArithmeticOverflow)?
    } else {
        old_debt
            .checked_sub(amount)
            .ok_or(ProgramError::ArithmeticOverflow)?
    };
    if borrow {
        let reserve_data = reserve.try_borrow_data()?;
        if reserve_data[96..128] != MULTIPLY_DEBT_FARM.to_bytes() {
            return Err(ProgramError::InvalidAccountData);
        }
        require_key(
            &accounts[7],
            &Pubkey::new_from_array(read_pubkey(&reserve_data[192..224])?),
        )?;
        require_key(&accounts[9], program_id)?;
        let fee_account = spl_token::state::Account::unpack(&accounts[7].try_borrow_data()?)?;
        let mint_account = spl_token::state::Mint::unpack(&mint.try_borrow_data()?)?;
        if accounts[7].owner != token.key
            || fee_account.mint != USDC_MINT
            || fee_account.owner != *authority.key
            || mint_account.decimals != 6
        {
            return Err(ProgramError::InvalidAccountData);
        }
        if !accounts[7].is_writable || next as u128 * 100 > collateral as u128 * 65 {
            return Err(ProgramError::InvalidArgument);
        }
        drop(reserve_data);
        transfer_checked_signed(
            program_id,
            supply,
            mint,
            custody,
            authority,
            token,
            amount,
            6,
            &[KAMINO_LENDING_MARKET_AUTHORITY_SEED, market.key.as_ref()],
        )?;
    } else {
        transfer_checked(custody, mint, supply, owner, token, amount, 6)?;
    }
    let mut data = obligation.try_borrow_mut_data()?;
    data[1208..1240].copy_from_slice(if next == 0 {
        &[0; 32]
    } else {
        reserve.key.as_ref()
    });
    data[1296..1312].copy_from_slice(&((next as u128) << 60).to_le_bytes());
    Ok(())
}

fn multiply_fixture_withdraw_amount(
    obligation: &AccountInfo,
    reserve: &AccountInfo,
    requested: u64,
) -> Result<u64, ProgramError> {
    if requested != u64::MAX {
        return Ok(requested);
    }
    let data = obligation.try_borrow_data()?;
    if data.len() != 3344 || data[32..64] != MULTIPLY_MARKET.to_bytes() {
        return Err(ProgramError::InvalidAccountData);
    }
    let collateral = read_u64(&data[128..136])?;
    drop(data);
    let debt = multiply_debt_raw(obligation)?;
    let reserve_data = reserve.try_borrow_data()?;
    // Source ReserveConfig liquidation threshold offset (locked SDK 23b9f2b),
    // explicitly initialized to80 by the public fixed-price fixture.
    if reserve_data.len() < 4874 || reserve_data[4873] != 80 {
        return Err(ProgramError::InvalidAccountData);
    }
    let locked = (debt as u128 * 100 + 79) / 80;
    let amount = (collateral as u128)
        .checked_sub(locked)
        .ok_or(ProgramError::InsufficientFunds)?;
    u64::try_from(amount).map_err(|_| ProgramError::ArithmeticOverflow)
}

fn process_multiply_shared_route(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    // Official Jupiter CPI IDL: id:u8, Vec<RoutePlanStep>; TokenSwap(enum3),
    // percent100,input0,output1; followed by amounts/u16 slippage/u8 fee.
    // Fixed-price model only: no real DEX/Jupiter routing or price claim.
    if accounts.len() != 13
        || data.len() != 36
        || data[8..17] != [0, 1, 0, 0, 0, 3, 100, 0, 1]
        || data[33..36] != [50, 0, 0]
    {
        return Err(ProgramError::InvalidInstructionData);
    }
    let amount = read_u64(&data[17..25])?;
    let quoted = read_u64(&data[25..33])?;
    if amount == 0 || quoted != amount {
        return Err(ProgramError::InvalidInstructionData);
    }
    require_key(&accounts[0], &spl_token::id())?;
    let authority = Pubkey::find_program_address(&[JUPITER_SWAP_AUTHORITY_SEED], program_id).0;
    require_key(&accounts[1], &authority)?;
    require_signer(&accounts[2])?;
    require_key(&accounts[9], program_id)?;
    require_key(&accounts[10], program_id)?;
    require_key(
        &accounts[11],
        &Pubkey::find_program_address(&[b"__event_authority"], program_id).0,
    )?;
    require_key(&accounts[12], program_id)?;
    if accounts
        .iter()
        .enumerate()
        .any(|(i, a)| i != 2 && a.is_signer)
        || [3, 4, 5, 6].iter().any(|i| !accounts[*i].is_writable)
        || !((accounts[7].key == &USDC_MINT && accounts[8].key == &MULTIPLY_SYRUP_MINT)
            || (accounts[8].key == &USDC_MINT && accounts[7].key == &MULTIPLY_SYRUP_MINT))
    {
        return Err(ProgramError::InvalidAccountData);
    }
    for (i, mint_i, owner) in [
        (3, 7, accounts[2].key),
        (4, 7, &authority),
        (5, 8, &authority),
        (6, 8, accounts[2].key),
    ] {
        let token = spl_token::state::Account::unpack(&accounts[i].try_borrow_data()?)?;
        if token.mint != *accounts[mint_i].key
            || token.owner != *owner
            || accounts[i].owner != &spl_token::id()
            || accounts[mint_i].owner != &spl_token::id()
        {
            return Err(ProgramError::InvalidAccountData);
        }
    }
    transfer_checked(
        &accounts[3],
        &accounts[7],
        &accounts[4],
        &accounts[2],
        &accounts[0],
        amount,
        6,
    )?;
    transfer_checked_signed(
        program_id,
        &accounts[5],
        &accounts[8],
        &accounts[6],
        &accounts[1],
        &accounts[0],
        amount,
        6,
        &[JUPITER_SWAP_AUTHORITY_SEED],
    )
}

fn parse_kamino_deposit_accounts<'a, 'info>(
    program_id: &Pubkey,
    accounts: &'a [AccountInfo<'info>],
) -> Result<KaminoDepositAccounts<'a, 'info>, ProgramError> {
    let account_info_iter = &mut accounts.iter();
    let owner = next_account_info(account_info_iter)?;
    let obligation = next_account_info(account_info_iter)?;
    let lending_market = next_account_info(account_info_iter)?;
    let lending_market_authority = next_account_info(account_info_iter)?;
    let reserve = next_account_info(account_info_iter)?;
    let reserve_liquidity_mint = next_account_info(account_info_iter)?;
    let reserve_liquidity_supply = next_account_info(account_info_iter)?;
    let reserve_collateral_mint = next_account_info(account_info_iter)?;
    let reserve_destination_deposit_collateral = next_account_info(account_info_iter)?;
    let user_source_liquidity = next_account_info(account_info_iter)?;
    let _placeholder_user_destination_collateral = next_account_info(account_info_iter)?;
    let collateral_token_program = next_account_info(account_info_iter)?;
    let liquidity_token_program = next_account_info(account_info_iter)?;
    let _instruction_sysvar_account = next_account_info(account_info_iter)?;
    let _obligation_farm_user_state = next_account_info(account_info_iter)?;
    let _reserve_farm_state = next_account_info(account_info_iter)?;
    let _farms_program = next_account_info(account_info_iter)?;
    let mut kamino = KaminoDepositAccounts {
        owner,
        obligation,
        reserve,
        lending_market,
        lending_market_authority,
        reserve_liquidity_mint,
        reserve_liquidity_supply,
        reserve_collateral_mint,
        user_source_liquidity,
        reserve_destination_deposit_collateral,
        collateral_token_program,
        liquidity_token_program,
        liquidity_decimals: 0,
    };

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
    require_key(
        kamino.reserve_destination_deposit_collateral,
        &reserve.collateral_supply,
    )?;

    kamino.liquidity_decimals =
        spl_token::state::Mint::unpack(&kamino.reserve_liquidity_mint.data.borrow())?.decimals;

    Ok(kamino)
}

fn parse_kamino_redeem_accounts<'a, 'info>(
    program_id: &Pubkey,
    accounts: &'a [AccountInfo<'info>],
) -> Result<KaminoRedeemAccounts<'a, 'info>, ProgramError> {
    let account_info_iter = &mut accounts.iter();
    let owner = next_account_info(account_info_iter)?;
    let obligation = next_account_info(account_info_iter)?;
    let lending_market = next_account_info(account_info_iter)?;
    let lending_market_authority = next_account_info(account_info_iter)?;
    let reserve = next_account_info(account_info_iter)?;
    let reserve_liquidity_mint = next_account_info(account_info_iter)?;
    let reserve_source_collateral = next_account_info(account_info_iter)?;
    let reserve_collateral_mint = next_account_info(account_info_iter)?;
    let reserve_liquidity_supply = next_account_info(account_info_iter)?;
    let user_destination_liquidity = next_account_info(account_info_iter)?;
    let _placeholder_user_destination_collateral = next_account_info(account_info_iter)?;
    let collateral_token_program = next_account_info(account_info_iter)?;
    let liquidity_token_program = next_account_info(account_info_iter)?;
    let _instruction_sysvar_account = next_account_info(account_info_iter)?;
    let _obligation_farm_user_state = next_account_info(account_info_iter)?;
    let _reserve_farm_state = next_account_info(account_info_iter)?;
    let _farms_program = next_account_info(account_info_iter)?;
    let mut kamino = KaminoRedeemAccounts {
        owner,
        obligation,
        lending_market,
        reserve,
        lending_market_authority,
        reserve_liquidity_mint,
        reserve_collateral_mint,
        reserve_liquidity_supply,
        reserve_source_collateral,
        user_destination_liquidity,
        collateral_token_program,
        liquidity_token_program,
        liquidity_decimals: 0,
    };

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
    require_key(kamino.reserve_source_collateral, &reserve.collateral_supply)?;

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

    if data.starts_with(&KAMINO_RESERVE_DISCRIMINATOR) {
        if data.len() < KAMINO_RESERVE_COLLATERAL_SUPPLY_OFFSET + 32 {
            return Err(ProgramError::InvalidAccountData);
        }
        return Ok(KaminoReserveState {
            lending_market: Pubkey::new_from_array(read_pubkey(
                &data[KAMINO_RESERVE_LENDING_MARKET_OFFSET
                    ..KAMINO_RESERVE_LENDING_MARKET_OFFSET + 32],
            )?),
            liquidity_mint: Pubkey::new_from_array(read_pubkey(
                &data[KAMINO_RESERVE_LIQUIDITY_MINT_OFFSET
                    ..KAMINO_RESERVE_LIQUIDITY_MINT_OFFSET + 32],
            )?),
            collateral_mint: Pubkey::new_from_array(read_pubkey(
                &data[KAMINO_RESERVE_COLLATERAL_MINT_OFFSET
                    ..KAMINO_RESERVE_COLLATERAL_MINT_OFFSET + 32],
            )?),
            liquidity_supply: Pubkey::new_from_array(read_pubkey(
                &data[KAMINO_RESERVE_LIQUIDITY_SUPPLY_OFFSET
                    ..KAMINO_RESERVE_LIQUIDITY_SUPPLY_OFFSET + 32],
            )?),
            collateral_supply: Pubkey::new_from_array(read_pubkey(
                &data[KAMINO_RESERVE_COLLATERAL_SUPPLY_OFFSET
                    ..KAMINO_RESERVE_COLLATERAL_SUPPLY_OFFSET + 32],
            )?),
        });
    }

    Ok(KaminoReserveState {
        lending_market: Pubkey::new_from_array(read_pubkey(&data[0..32])?),
        liquidity_mint: Pubkey::new_from_array(read_pubkey(&data[32..64])?),
        collateral_mint: Pubkey::new_from_array(read_pubkey(&data[64..96])?),
        liquidity_supply: Pubkey::new_from_array(read_pubkey(&data[96..128])?),
        collateral_supply: Pubkey::new_from_array(read_pubkey(&data[128..160])?),
    })
}

fn adjust_kamino_obligation(
    obligation: &AccountInfo,
    reserve: &Pubkey,
    amount: u64,
    deposit: bool,
) -> ProgramResult {
    if obligation.owner != &KAMINO_LEND_PROGRAM_ID {
        return Err(ProgramError::IllegalOwner);
    }
    let mut data = obligation.try_borrow_mut_data()?;
    if data.len() < KAMINO_OBLIGATION_FIRST_DEPOSIT_AMOUNT_OFFSET + 8
        || !data.starts_with(&KAMINO_OBLIGATION_DISCRIMINATOR)
    {
        return Err(ProgramError::InvalidAccountData);
    }
    let reserve_range = KAMINO_OBLIGATION_FIRST_DEPOSIT_RESERVE_OFFSET
        ..KAMINO_OBLIGATION_FIRST_DEPOSIT_RESERVE_OFFSET + 32;
    let current_reserve = Pubkey::new_from_array(read_pubkey(&data[reserve_range.clone()])?);
    if current_reserve != Pubkey::default() && current_reserve != *reserve {
        return Err(ProgramError::InvalidAccountData);
    }
    data[reserve_range].copy_from_slice(reserve.as_ref());
    let amount_range = KAMINO_OBLIGATION_FIRST_DEPOSIT_AMOUNT_OFFSET
        ..KAMINO_OBLIGATION_FIRST_DEPOSIT_AMOUNT_OFFSET + 8;
    let current_amount = read_u64(&data[amount_range.clone()])?;
    let next_amount = if deposit {
        current_amount
            .checked_add(amount)
            .ok_or(ProgramError::ArithmeticOverflow)?
    } else {
        current_amount
            .checked_sub(amount)
            .ok_or(ProgramError::InsufficientFunds)?
    };
    data[amount_range].copy_from_slice(&next_amount.to_le_bytes());
    Ok(())
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

#[allow(clippy::too_many_arguments)]
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

#[allow(clippy::too_many_arguments)]
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

#[allow(clippy::too_many_arguments)]
fn burn_checked_signed<'info>(
    program_id: &Pubkey,
    source: &AccountInfo<'info>,
    mint: &AccountInfo<'info>,
    authority: &AccountInfo<'info>,
    token_program: &AccountInfo<'info>,
    amount: u64,
    decimals: u8,
    seed_parts: &[&[u8]],
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
    let account_infos = [
        source.clone(),
        mint.clone(),
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

fn read_u64_at(data: &[u8], offset: u64) -> Result<u64, ProgramError> {
    let offset = offset as usize;
    read_u64(&data[offset..offset + 8])
}

fn read_pubkey(data: &[u8]) -> Result<[u8; 32], ProgramError> {
    data.try_into()
        .map_err(|_| ProgramError::InvalidInstructionData)
}
