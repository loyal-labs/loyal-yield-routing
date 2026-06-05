use borsh::BorshSerialize;
use loyal_actions::{
    kamino_deposit_reserve_liquidity_instruction, kamino_withdraw_reserve_liquidity_instruction,
    KaminoDepositReserveLiquidityAccounts, KaminoWithdrawReserveLiquidityAccounts,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use serde::Deserialize;
use serde_json::{json, Value};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey,
    pubkey::Pubkey,
};
use std::str::FromStr;
use thiserror::Error;

use crate::planner::{KaminoReserveAccountsConfig, SameMintQuote, SameMintReserveTarget};

const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
const ASSOCIATED_TOKEN_PROGRAM_ID: Pubkey = pubkey!("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");

#[derive(Debug, Error)]
pub enum RouteBuildError {
    #[error("execution plan is not a same_mint plan")]
    WrongPlanKind,
    #[error("missing execution plan field: {0}")]
    MissingField(&'static str),
    #[error("invalid pubkey in {field}: {value}")]
    InvalidPubkey { field: &'static str, value: String },
    #[error("invalid numeric field {field}: {value}")]
    InvalidNumber { field: &'static str, value: i64 },
    #[error("execution plan json error: {0}")]
    Json(#[from] serde_json::Error),
}

#[derive(Debug, Clone)]
pub struct SameMintRouteTransaction {
    pub instruction: Instruction,
    pub report: Value,
}

#[derive(Debug, Clone, Deserialize)]
struct SameMintExecutionPlan {
    kind: String,
    route: SameMintRouteFields,
    quote: SameMintQuote,
    source: SameMintReserveTarget,
    target: SameMintReserveTarget,
}

#[derive(Debug, Clone, Deserialize)]
struct SameMintRouteFields {
    policy_account: String,
    vault_pubkey: String,
    vault_index: i16,
    withdraw_constraint_index: u8,
    deposit_constraint_index: u8,
}

#[derive(Debug)]
struct SquadsCompiledInstruction {
    program_id_index: usize,
    accounts: Vec<usize>,
    data: Vec<u8>,
}

#[derive(BorshSerialize)]
struct SquadsSyncTransactionArgs {
    account_index: u8,
    num_signers: u8,
    payload: SquadsSyncPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(SquadsPolicyPayload),
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyPayload {
    InternalFundTransfer(Vec<u8>),
    ProgramInteraction(SquadsProgramInteractionPayload),
    SpendingLimit(Vec<u8>),
    SettingsChange(Vec<u8>),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPayload {
    instruction_constraint_indices: Option<Vec<u8>>,
    transaction_payload: SquadsProgramInteractionTransactionPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsProgramInteractionTransactionPayload {
    AsyncTransaction(Vec<u8>),
    SyncTransaction(SquadsProgramInteractionSyncPayload),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionSyncPayload {
    account_index: u8,
    instructions: Vec<u8>,
}

pub fn build_same_mint_route_transaction(
    execution_plan: &Value,
    delegated_signer: Pubkey,
) -> Result<SameMintRouteTransaction, RouteBuildError> {
    let plan: SameMintExecutionPlan = serde_json::from_value(execution_plan.clone())?;
    if plan.kind != "same_mint" {
        return Err(RouteBuildError::WrongPlanKind);
    }

    let policy = parse_pubkey("route.policy_account", &plan.route.policy_account)?;
    let vault = parse_pubkey("route.vault_pubkey", &plan.route.vault_pubkey)?;
    let vault_index =
        u8::try_from(plan.route.vault_index).map_err(|_| RouteBuildError::InvalidNumber {
            field: "route.vault_index",
            value: i64::from(plan.route.vault_index),
        })?;

    let withdraw = kamino_withdraw_reserve_liquidity_instruction(
        withdraw_accounts(vault, &plan.source)?,
        plan.quote.redeem_collateral_amount,
    );
    let deposit = kamino_deposit_reserve_liquidity_instruction(
        deposit_accounts(vault, &plan.target)?,
        plan.quote.deposit_liquidity_amount,
    );

    let mut transaction_accounts = Vec::new();
    let compiled_instructions = vec![
        compile_inner_instruction(&mut transaction_accounts, withdraw),
        compile_inner_instruction(&mut transaction_accounts, deposit),
    ];
    let instruction = execute_squads_program_interaction_instruction(
        policy,
        delegated_signer,
        vault_index,
        compiled_instructions,
        vec![
            plan.route.withdraw_constraint_index,
            plan.route.deposit_constraint_index,
        ],
        transaction_accounts,
    );

    Ok(SameMintRouteTransaction {
        report: json!({
            "kind": "same_mint_route_transaction",
            "policy": policy.to_string(),
            "vault": vault.to_string(),
            "vaultIndex": vault_index,
            "delegatedSigner": delegated_signer.to_string(),
            "withdrawConstraintIndex": plan.route.withdraw_constraint_index,
            "depositConstraintIndex": plan.route.deposit_constraint_index,
            "quote": plan.quote,
        }),
        instruction,
    })
}

pub fn associated_token_address(wallet: Pubkey, token_program: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[wallet.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn withdraw_accounts(
    vault: Pubkey,
    target: &SameMintReserveTarget,
) -> Result<KaminoWithdrawReserveLiquidityAccounts, RouteBuildError> {
    let token_program = token_program(&target.accounts)?;
    let liquidity_mint = parse_pubkey("source.liquidity_mint", &target.liquidity_mint)?;
    let collateral_mint = parse_pubkey(
        "source.accounts.reserve_collateral_mint",
        &target.accounts.reserve_collateral_mint,
    )?;
    Ok(KaminoWithdrawReserveLiquidityAccounts {
        owner: vault,
        lending_market: parse_pubkey("source.market", &target.market)?,
        reserve: parse_pubkey("source.reserve", &target.reserve)?,
        lending_market_authority: parse_pubkey(
            "source.accounts.lending_market_authority",
            &target.accounts.lending_market_authority,
        )?,
        reserve_liquidity_mint: liquidity_mint,
        reserve_collateral_mint: collateral_mint,
        reserve_liquidity_supply: parse_pubkey(
            "source.accounts.reserve_liquidity_supply",
            &target.accounts.reserve_liquidity_supply,
        )?,
        user_source_collateral: associated_token_address(vault, token_program, collateral_mint),
        user_destination_liquidity: associated_token_address(vault, token_program, liquidity_mint),
        liquidity_token_program: token_program,
    })
}

fn deposit_accounts(
    vault: Pubkey,
    target: &SameMintReserveTarget,
) -> Result<KaminoDepositReserveLiquidityAccounts, RouteBuildError> {
    let token_program = token_program(&target.accounts)?;
    let liquidity_mint = parse_pubkey("target.liquidity_mint", &target.liquidity_mint)?;
    let collateral_mint = parse_pubkey(
        "target.accounts.reserve_collateral_mint",
        &target.accounts.reserve_collateral_mint,
    )?;
    Ok(KaminoDepositReserveLiquidityAccounts {
        owner: vault,
        reserve: parse_pubkey("target.reserve", &target.reserve)?,
        lending_market: parse_pubkey("target.market", &target.market)?,
        lending_market_authority: parse_pubkey(
            "target.accounts.lending_market_authority",
            &target.accounts.lending_market_authority,
        )?,
        reserve_liquidity_mint: liquidity_mint,
        reserve_liquidity_supply: parse_pubkey(
            "target.accounts.reserve_liquidity_supply",
            &target.accounts.reserve_liquidity_supply,
        )?,
        reserve_collateral_mint: collateral_mint,
        user_source_liquidity: associated_token_address(vault, token_program, liquidity_mint),
        user_destination_collateral: associated_token_address(
            vault,
            token_program,
            collateral_mint,
        ),
        liquidity_token_program: token_program,
    })
}

fn token_program(accounts: &KaminoReserveAccountsConfig) -> Result<Pubkey, RouteBuildError> {
    accounts
        .liquidity_token_program
        .as_deref()
        .map(|value| parse_pubkey("accounts.liquidity_token_program", value))
        .unwrap_or(Ok(spl_token::ID))
}

fn execute_squads_program_interaction_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(policy, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_policy_payload_args(
            account_index,
            SquadsPolicyPayload::ProgramInteraction(SquadsProgramInteractionPayload {
                instruction_constraint_indices: Some(instruction_constraint_indices),
                transaction_payload: SquadsProgramInteractionTransactionPayload::SyncTransaction(
                    SquadsProgramInteractionSyncPayload {
                        account_index,
                        instructions: squads_compiled_instruction_payload(&compiled_instructions),
                    },
                ),
            }),
        ),
    }
}

fn serialize_squads_sync_policy_payload_args(
    account_index: u8,
    policy_payload: SquadsPolicyPayload,
) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Policy(policy_payload),
    }
    .serialize(&mut data)
    .expect("serialize Squads sync policy payload");
    data
}

fn squads_compiled_instruction_payload(instructions: &[SquadsCompiledInstruction]) -> Vec<u8> {
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

fn compile_inner_instruction(
    transaction_accounts: &mut Vec<AccountMeta>,
    instruction: Instruction,
) -> SquadsCompiledInstruction {
    let accounts = instruction
        .accounts
        .into_iter()
        .map(|account| push_or_update_account_meta(transaction_accounts, account))
        .collect();
    let program_id_index = push_or_update_account_meta(
        transaction_accounts,
        AccountMeta::new_readonly(instruction.program_id, false),
    );

    SquadsCompiledInstruction {
        program_id_index,
        accounts,
        data: instruction.data,
    }
}

fn push_or_update_account_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> usize {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_writable |= meta.is_writable;
        accounts[index].is_signer |= meta.is_signer;
        return index;
    }

    let index = accounts.len();
    accounts.push(meta);
    index
}

fn parse_pubkey(field: &'static str, value: &str) -> Result<Pubkey, RouteBuildError> {
    Pubkey::from_str(value).map_err(|_| RouteBuildError::InvalidPubkey {
        field,
        value: value.to_owned(),
    })
}
