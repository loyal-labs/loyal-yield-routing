//! Offline, one-operation, zero-fee ONyc proof. No deployment wiring.
#![allow(unexpected_cfgs)]
mod contract;
use contract::*;
use solana_program::{
    account_info::AccountInfo,
    clock::Clock,
    entrypoint::ProgramResult,
    instruction::{get_stack_height, AccountMeta, Instruction},
    program::{invoke, invoke_signed},
    program_error::ProgramError,
    program_pack::Pack,
    pubkey::Pubkey,
    rent::Rent,
    sysvar::{
        instructions::{load_current_index_checked, load_instruction_at_checked},
        Sysvar,
    },
};
use spl_token::state::{Account as TokenAccount, AccountState, Mint};

#[cfg(not(feature = "no-entrypoint"))]
solana_program::entrypoint!(process_instruction);

type Result<T> = std::result::Result<T, ProgramError>;
const DATA: u32 = 7000;
const ACCOUNTS: u32 = 7001;
const GRAMMAR: u32 = 7002;
const AUTHORITY: u32 = 7003;
const RECEIPT: u32 = 7004;
const POSITION: u32 = 7005;
const PAYMENT: u32 = 7006;
const FEE: u32 = 7007;
const PROCEEDS: u32 = 7008;
const CLOSURE: u32 = 7009;

fn require(ok: bool, code: u32) -> ProgramResult {
    if ok {
        Ok(())
    } else {
        Err(ProgramError::Custom(code))
    }
}
fn u64_at(data: &[u8], offset: usize) -> Result<u64> {
    let bytes = data
        .get(offset..offset + 8)
        .ok_or(ProgramError::Custom(DATA))?;
    Ok(u64::from_le_bytes(
        bytes.try_into().map_err(|_| ProgramError::Custom(DATA))?,
    ))
}
fn receipt_address() -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            RECEIPT_SEED,
            OBLIGATION.as_ref(),
            EXECUTOR.as_ref(),
            &OPERATION.to_le_bytes(),
        ],
        &PROGRAM,
    )
}
fn settlement_policy() -> Pubkey {
    // This seed was created by the captured Squads program in candidate-policy.json.
    Pubkey::find_program_address(
        &[
            b"smart_account",
            b"policy",
            SETTINGS.as_ref(),
            &157u64.to_le_bytes(),
        ],
        &SQUADS,
    )
    .0
}
fn settle_keys() -> [Pubkey; 10] {
    [
        receipt_address().0,
        VAULT,
        CASH,
        REPAY_KEYS[6],
        USDC,
        spl_token::id(),
        OBLIGATION,
        ONYC,
        solana_program::sysvar::instructions::ID,
        EXECUTOR,
    ]
}
fn begin_keys() -> Vec<Pubkey> {
    let mut keys = REPAY_KEYS.to_vec();
    keys.extend([receipt_address().0, CASH, VAULT, ONYC, SYSTEM, KLEND]);
    keys
}
fn same_keys(ix: &Instruction, keys: &[Pubkey]) -> bool {
    ix.accounts.len() == keys.len() && ix.accounts.iter().zip(keys).all(|(a, k)| a.pubkey == *k)
}
fn check_infos(accounts: &[AccountInfo], keys: &[Pubkey]) -> ProgramResult {
    require(
        accounts.len() == keys.len() && accounts.iter().zip(keys).all(|(a, k)| a.key == k),
        ACCOUNTS,
    )
}
fn plain_token(account: &AccountInfo, mint: Pubkey, authority: Pubkey) -> Result<TokenAccount> {
    require(
        account.owner == &spl_token::id()
            && !account.executable
            && account.data_len() == TokenAccount::LEN,
        ACCOUNTS,
    )?;
    let token = TokenAccount::unpack(&account.try_borrow_data()?)?;
    require(
        token.state == AccountState::Initialized
            && token.mint == mint
            && token.owner == authority
            && token.delegate.is_none()
            && token.delegated_amount == 0
            && token.close_authority.is_none()
            && token.is_native.is_none(),
        AUTHORITY,
    )?;
    Ok(token)
}
fn cash(account: &AccountInfo, authority: Pubkey) -> Result<u64> {
    Ok(plain_token(account, USDC, authority)?.amount)
}
fn mint(account: &AccountInfo) -> ProgramResult {
    require(
        account.key == &USDC
            && account.owner == &spl_token::id()
            && account.data_len() == Mint::LEN
            && !account.executable,
        ACCOUNTS,
    )?;
    let mint = Mint::unpack(&account.try_borrow_data()?)?;
    require(mint.is_initialized && mint.decimals == 6, ACCOUNTS)
}
fn position(account: &AccountInfo, indebted: bool) -> ProgramResult {
    require(
        account.key == &OBLIGATION
            && account.owner == &KLEND
            && !account.executable
            && account.data_len() == 3344,
        POSITION,
    )?;
    let d = account.try_borrow_data()?;
    require(
        d[..8] == [168, 206, 141, 106, 88, 76, 172, 167]
            && d[32..64] == MARKET.to_bytes()
            && d[64..96] == VAULT.to_bytes(),
        POSITION,
    )?;
    let mut deposits = 0;
    for i in 0..8 {
        let start = 96 + i * 136;
        let reserve = &d[start..start + 32];
        let amount = u64_at(&d, start + 32)?;
        if reserve == COLLATERAL.as_ref() && amount > 0 {
            deposits += 1;
        } else {
            require(reserve == [0; 32] && amount == 0, POSITION)?;
        }
    }
    require(deposits == 1, POSITION)?;
    let mut debts = 0;
    for i in 0..5 {
        let start = 1208 + i * 200;
        let reserve = &d[start..start + 32];
        let amount = &d[start + 88..start + 104];
        if indebted && reserve == DEBT.as_ref() && amount != [0; 16] {
            debts += 1;
        } else {
            require(reserve == [0; 32] && amount == [0; 16], POSITION)?;
        }
    }
    require(debts == usize::from(indebted), POSITION)
}

// Decode only the single-inner, one-constraint Sync V2 encoding used by the SDK.
// Reject alternative variants, malformed lengths, extra calls and trailing data.
fn squads_inner(ix: &Instruction, policy: Pubkey, constraint: u8) -> Result<Instruction> {
    let d = &ix.data;
    require(
        ix.program_id == SQUADS
            && ix.accounts.len() >= 4
            && ix.accounts[0].pubkey == policy
            && ix.accounts[1].pubkey == SQUADS
            && ix.accounts[2].pubkey == EXECUTOR
            && ix.accounts[2].is_signer,
        GRAMMAR,
    )?;
    require(
        d.len() >= 29
            && d[..8] == [90, 81, 187, 81, 39, 70, 128, 78]
            && d[8..13] == [0, 1, 1, 1, 1]
            && d[13..17] == [1, 0, 0, 0]
            && d[17] == constraint
            && d[18..20] == [1, 0],
        GRAMMAR,
    )?;
    let payload_len = u32::from_le_bytes(
        d[20..24]
            .try_into()
            .map_err(|_| ProgramError::Custom(GRAMMAR))?,
    ) as usize;
    require(payload_len == d.len() - 24 && d[24] == 1, GRAMMAR)?;
    let n = d[26] as usize;
    require(n <= 64 && d.len() >= 29 + n, GRAMMAR)?;
    let inner_len = u16::from_le_bytes(
        d[27 + n..29 + n]
            .try_into()
            .map_err(|_| ProgramError::Custom(GRAMMAR))?,
    ) as usize;
    require(d.len() == 29 + n + inner_len, GRAMMAR)?;
    let transaction_accounts = &ix.accounts[3..];
    require(transaction_accounts.len() <= 65, GRAMMAR)?;
    // Canonical SDK encoding: unique outer keys, first-use inner indices in
    // order, followed by the program index. No unused outer account remains.
    for (i, account) in transaction_accounts.iter().enumerate() {
        require(
            !transaction_accounts[..i]
                .iter()
                .any(|a| a.pubkey == account.pubkey),
            GRAMMAR,
        )?;
    }
    let mut next = 0usize;
    for index in d[27..27 + n].iter().chain(std::iter::once(&d[25])) {
        require((*index as usize) <= next, GRAMMAR)?;
        if *index as usize == next {
            next += 1;
        }
    }
    require(next == transaction_accounts.len(), GRAMMAR)?;
    let program = transaction_accounts
        .get(d[25] as usize)
        .ok_or(ProgramError::Custom(GRAMMAR))?
        .pubkey;
    let accounts = d[27..27 + n]
        .iter()
        .map(|index| {
            transaction_accounts
                .get(*index as usize)
                .cloned()
                .ok_or(ProgramError::Custom(GRAMMAR))
        })
        .collect::<Result<Vec<_>>>()?;
    Ok(Instruction {
        program_id: program,
        accounts,
        data: d[29 + n..].to_vec(),
    })
}
fn check_begin(ix: &Instruction) -> ProgramResult {
    require(
        ix.program_id == PROGRAM && same_keys(ix, &begin_keys()) && ix.accounts[0].is_signer,
        GRAMMAR,
    )?;
    require(
        ix.data.len() == 40 && &ix.data[..8] == BEGIN && u64_at(&ix.data, 8)? == OPERATION,
        DATA,
    )?;
    let requested = u64_at(&ix.data, 16)?;
    require(requested > 0 && requested <= MAX_PRINCIPAL, PAYMENT)?;
    require(u64_at(&ix.data, 24)? == 0, FEE)
}
fn check_refresh(ix: &Instruction, keys: &[Pubkey], data: &[u8]) -> ProgramResult {
    require(
        ix.program_id == KLEND && same_keys(ix, keys) && ix.data == data,
        GRAMMAR,
    )
}
fn transaction(sysvar: &AccountInfo) -> ProgramResult {
    require(
        sysvar.key == &solana_program::sysvar::instructions::ID,
        ACCOUNTS,
    )?;
    {
        let d = sysvar.try_borrow_data()?;
        require(
            d.len() >= 2 && u16::from_le_bytes([d[0], d[1]]) == 10,
            GRAMMAR,
        )?;
    }
    let ix = |index| load_instruction_at_checked(index, sysvar);
    check_refresh(&ix(0)?, &REFRESH_COLLATERAL_KEYS, &REFRESH_RESERVE)?;
    check_refresh(&ix(1)?, &REFRESH_DEBT_KEYS, &REFRESH_RESERVE)?;
    check_refresh(&ix(2)?, &REFRESH_BEFORE_KEYS, &REFRESH_OBLIGATION)?;
    check_begin(&ix(3)?)?;
    check_refresh(&ix(4)?, &REFRESH_COLLATERAL_KEYS, &REFRESH_RESERVE)?;
    check_refresh(&ix(5)?, &REFRESH_DEBT_KEYS, &REFRESH_RESERVE)?;
    check_refresh(&ix(6)?, &REFRESH_AFTER_KEYS, &REFRESH_OBLIGATION)?;
    let withdraw = squads_inner(&ix(7)?, WITHDRAW_POLICY, 1)?;
    require(
        withdraw.program_id == KLEND
            && same_keys(&withdraw, &WITHDRAW_KEYS)
            && withdraw.data.len() == 16
            && withdraw.data[..8] == WITHDRAW_DISC
            && u64_at(&withdraw.data, 8)? > 0,
        GRAMMAR,
    )?;
    let swap = squads_inner(&ix(8)?, SWAP_POLICY, 0)?;
    require(
        swap.program_id == JUPITER
            && same_keys(&swap, &SWAP_KEYS)
            && swap.data.len() == 38
            && swap.data[..8] == [193, 32, 155, 51, 65, 214, 156, 129]
            && swap.data[8] == 7
            && swap.data[9..14] == [1, 0, 0, 0, 47]
            && swap.data[14] == 1
            && swap.data[15..19] == [0, 100, 0, 1]
            && swap.data[37] == 0
            && u64_at(&swap.data, 19)? > 0
            && u64_at(&swap.data, 27)? > 0
            && u16::from_le_bytes([swap.data[35], swap.data[36]]) <= 50,
        GRAMMAR,
    )?;
    let settle = squads_inner(&ix(9)?, settlement_policy(), 0)?;
    require(
        settle.program_id == PROGRAM
            && same_keys(&settle, &settle_keys())
            && settle.data.len() == 16
            && &settle.data[..8] == SETTLE
            && u64_at(&settle.data, 8)? == OPERATION,
        GRAMMAR,
    )
}

pub fn process_instruction(
    program: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    require(program == &PROGRAM && data.len() >= 8, DATA)?;
    if &data[..8] == BEGIN {
        begin(accounts, data)
    } else if &data[..8] == SETTLE {
        settle(accounts, data)
    } else {
        Err(ProgramError::Custom(DATA))
    }
}
fn begin(accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    check_infos(accounts, &begin_keys())?;
    require(get_stack_height() == 1 && accounts[0].is_signer, AUTHORITY)?;
    let sysvar = &accounts[8];
    require(load_current_index_checked(sysvar)? == 3, GRAMMAR)?;
    transaction(sysvar)?;
    let current = load_instruction_at_checked(3, sysvar)?;
    require(current.data == data, GRAMMAR)?;
    let receipt = &accounts[14];
    require(
        receipt.owner == &SYSTEM
            && receipt.data_is_empty()
            && receipt.lamports() == 0
            && !receipt.executable,
        RECEIPT,
    )?;
    mint(&accounts[4])?;
    let x_before = cash(&accounts[6], EXECUTOR)?;
    let u_before = cash(&accounts[15], VAULT)?;
    position(&accounts[1], true)?;
    let (address, bump) = receipt_address();
    let nonce = OPERATION.to_le_bytes();
    let bump_bytes = [bump];
    let seeds: &[&[u8]] = &[
        RECEIPT_SEED,
        OBLIGATION.as_ref(),
        EXECUTOR.as_ref(),
        &nonce,
        &bump_bytes,
    ];
    let create = solana_system_interface::instruction::create_account(
        &EXECUTOR,
        &address,
        Rent::get()?.minimum_balance(RECEIPT_LEN),
        RECEIPT_LEN as u64,
        &PROGRAM,
    );
    invoke_signed(
        &create,
        &[accounts[0].clone(), receipt.clone(), accounts[18].clone()],
        &[seeds],
    )?;
    let mut repay_data = REPAY_DISC.to_vec();
    repay_data.extend(u64_at(data, 16)?.to_le_bytes());
    let repay = Instruction {
        program_id: KLEND,
        accounts: accounts[..14]
            .iter()
            .map(|a| AccountMeta {
                pubkey: *a.key,
                is_signer: a.is_signer,
                is_writable: a.is_writable,
            })
            .collect(),
        data: repay_data,
    };
    invoke(&repay, accounts)?;
    position(&accounts[1], false)?;
    let paid = x_before
        .checked_sub(cash(&accounts[6], EXECUTOR)?)
        .ok_or(ProgramError::Custom(PAYMENT))?;
    require(
        paid > 0 && paid <= MAX_PRINCIPAL && cash(&accounts[15], VAULT)? == u_before,
        PAYMENT,
    )?;
    let mut record = receipt.try_borrow_mut_data()?;
    require(
        record.len() == RECEIPT_LEN && receipt.owner == &PROGRAM,
        RECEIPT,
    )?;
    record.fill(0);
    record[..8].copy_from_slice(b"ONYCRCP1");
    record[8] = 1;
    record[9..11].copy_from_slice(&3u16.to_le_bytes());
    record[11..19].copy_from_slice(&Clock::get()?.slot.to_le_bytes());
    record[19..27].copy_from_slice(&paid.to_le_bytes());
    record[27..35].copy_from_slice(&u_before.to_le_bytes());
    record[35..43].copy_from_slice(&x_before.to_le_bytes());
    record[43..51].copy_from_slice(&OPERATION.to_le_bytes());
    record[51..59].copy_from_slice(&u64_at(data, 32)?.to_le_bytes());
    Ok(())
}
fn settle(accounts: &[AccountInfo], data: &[u8]) -> ProgramResult {
    check_infos(accounts, &settle_keys())?;
    require(
        data.len() == 16 && &data[..8] == SETTLE && u64_at(data, 8)? == OPERATION,
        DATA,
    )?;
    require(get_stack_height() == 2 && accounts[1].is_signer, AUTHORITY)?;
    let sysvar = &accounts[8];
    require(load_current_index_checked(sysvar)? == 9, GRAMMAR)?;
    transaction(sysvar)?;
    let receipt = &accounts[0];
    require(
        receipt.owner == &PROGRAM
            && !receipt.executable
            && receipt.data_len() == RECEIPT_LEN
            && Rent::get()?.is_exempt(receipt.lamports(), RECEIPT_LEN),
        RECEIPT,
    )?;
    let (paid, u_initial, x_initial, min_retained) = {
        let record = receipt.try_borrow_data()?;
        require(
            &record[..8] == b"ONYCRCP1"
                && record[8] == 1
                && record[9..11] == [3, 0]
                && u64_at(&record, 11)? == Clock::get()?.slot
                && u64_at(&record, 43)? == OPERATION
                && record[59..] == [0; 5],
            RECEIPT,
        )?;
        (
            u64_at(&record, 19)?,
            u64_at(&record, 27)?,
            u64_at(&record, 35)?,
            u64_at(&record, 51)?,
        )
    };
    let earlier = load_instruction_at_checked(3, sysvar)?;
    require(
        u64_at(&earlier.data, 32)? == min_retained
            && paid > 0
            && paid <= MAX_PRINCIPAL
            && paid <= u64_at(&earlier.data, 16)?,
        RECEIPT,
    )?;
    // Only the captured full-exit outcome is supported: the bound KLend
    // withdrawal closed this previously authenticated live obligation.
    let obligation = &accounts[6];
    require(
        obligation.owner == &SYSTEM
            && obligation.data_is_empty()
            && obligation.lamports() == 0
            && !obligation.executable,
        CLOSURE,
    )?;
    require(
        plain_token(&accounts[7], SWAP_KEYS[7], VAULT)?.amount == 0,
        CLOSURE,
    )?;
    mint(&accounts[4])?;
    let u_before = cash(&accounts[2], VAULT)?;
    let x_before = cash(&accounts[3], EXECUTOR)?;
    let retained = u_before
        .checked_sub(paid)
        .ok_or(ProgramError::Custom(PROCEEDS))?;
    require(
        retained
            >= u_initial
                .checked_add(min_retained)
                .ok_or(ProgramError::Custom(PROCEEDS))?
            && x_before.checked_add(paid) == Some(x_initial),
        PROCEEDS,
    )?;
    receipt.try_borrow_mut_data()?[8] = 2;
    let transfer = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &CASH,
        &USDC,
        &REPAY_KEYS[6],
        &VAULT,
        &[],
        paid,
        6,
    )?;
    invoke(
        &transfer,
        &[
            accounts[2].clone(),
            accounts[4].clone(),
            accounts[3].clone(),
            accounts[1].clone(),
            accounts[5].clone(),
        ],
    )?;
    require(
        cash(&accounts[2], VAULT)? == retained && cash(&accounts[3], EXECUTOR)? == x_initial,
        PAYMENT,
    )
}
