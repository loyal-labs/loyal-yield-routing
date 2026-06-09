use borsh::BorshSerialize;
use loyal_actions::{
    kamino_deposit_reserve_liquidity_instruction, kamino_withdraw_reserve_liquidity_instruction,
    KaminoDepositReserveLiquidityAccounts, KaminoWithdrawReserveLiquidityAccounts,
    KAMINO_LENDING_PROGRAM_ID, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
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

use crate::planner::{
    CrossMintQuote, KaminoReserveAccountsConfig, RouteInstructionConfig, SameMintQuote,
    SameMintReserveTarget,
};

const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
const ASSOCIATED_TOKEN_PROGRAM_ID: Pubkey = pubkey!("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");

#[derive(Debug, Error)]
pub enum RouteBuildError {
    #[error("execution plan kind is not supported")]
    WrongPlanKind,
    #[error("missing execution plan field: {0}")]
    MissingField(&'static str),
    #[error("invalid pubkey in {field}: {value}")]
    InvalidPubkey { field: &'static str, value: String },
    #[error("invalid numeric field {field}: {value}")]
    InvalidNumber { field: &'static str, value: i64 },
    #[error(
        "split cross-mint swap policies are not supported; install a unified compact route policy"
    )]
    SplitSwapPolicyUnsupported,
    #[error("execution plan json error: {0}")]
    Json(#[from] serde_json::Error),
}

#[derive(Debug, Clone)]
pub struct YieldRouteTransaction {
    pub instructions: Vec<Instruction>,
    pub preflight_accounts: Vec<RoutePreflightAccount>,
    pub report: Value,
}

pub type SameMintRouteTransaction = YieldRouteTransaction;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RoutePreflightAccount {
    pub label: String,
    pub address: Pubkey,
    pub owner_program: Pubkey,
}

#[derive(Debug, Clone)]
pub struct KaminoDepositSyncTransaction {
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
struct CrossMintExecutionPlan {
    kind: String,
    route: CrossMintRouteFields,
    quote: CrossMintQuote,
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

#[derive(Debug, Clone, Deserialize)]
struct CrossMintRouteFields {
    policy_account: String,
    #[serde(default)]
    swap_policy_account: Option<String>,
    vault_pubkey: String,
    vault_index: i16,
    withdraw_constraint_index: u8,
    swap_constraint_index: u8,
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
    build_same_mint_route_transaction_inner(execution_plan, delegated_signer)
}

pub fn build_yield_route_transaction(
    execution_plan: &Value,
    delegated_signer: Pubkey,
) -> Result<YieldRouteTransaction, RouteBuildError> {
    match execution_plan.get("kind").and_then(Value::as_str) {
        Some("same_mint") => {
            build_same_mint_route_transaction_inner(execution_plan, delegated_signer)
        }
        Some("cross_mint") => build_cross_mint_route_transaction(execution_plan, delegated_signer),
        _ => Err(RouteBuildError::WrongPlanKind),
    }
}

fn build_same_mint_route_transaction_inner(
    execution_plan: &Value,
    delegated_signer: Pubkey,
) -> Result<YieldRouteTransaction, RouteBuildError> {
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
        compile_squads_vault_instruction(&mut transaction_accounts, vault, withdraw),
        compile_squads_vault_instruction(&mut transaction_accounts, vault, deposit),
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
    let preflight_accounts = required_token_accounts(vault, &plan.source, &plan.target)?;

    Ok(YieldRouteTransaction {
        report: json!({
            "kind": "same_mint_route_transaction",
            "policy": policy.to_string(),
            "vault": vault.to_string(),
            "vaultIndex": vault_index,
            "delegatedSigner": delegated_signer.to_string(),
            "withdrawConstraintIndex": plan.route.withdraw_constraint_index,
            "depositConstraintIndex": plan.route.deposit_constraint_index,
            "quote": plan.quote,
            "preflightAccounts": preflight_accounts_json(&preflight_accounts),
        }),
        preflight_accounts,
        instructions: vec![instruction],
    })
}

pub fn build_cross_mint_route_transaction(
    execution_plan: &Value,
    delegated_signer: Pubkey,
) -> Result<YieldRouteTransaction, RouteBuildError> {
    let plan: CrossMintExecutionPlan = serde_json::from_value(execution_plan.clone())?;
    if plan.kind != "cross_mint" {
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
    let swap = plan
        .quote
        .swap
        .instruction
        .as_ref()
        .ok_or(RouteBuildError::MissingField("quote.swap.instruction"))
        .and_then(swap_instruction)?;
    let deposit = kamino_deposit_reserve_liquidity_instruction(
        deposit_accounts(vault, &plan.target)?,
        plan.quote.deposit_liquidity_amount,
    );

    let swap_policy = plan
        .route
        .swap_policy_account
        .as_deref()
        .map(|value| parse_pubkey("route.swap_policy_account", value))
        .transpose()?;
    if swap_policy.is_some() {
        return Err(RouteBuildError::SplitSwapPolicyUnsupported);
    }
    let mut transaction_accounts = Vec::new();
    let compiled_instructions = vec![
        compile_squads_vault_instruction(&mut transaction_accounts, vault, withdraw),
        compile_squads_vault_instruction(&mut transaction_accounts, vault, swap),
        compile_squads_vault_instruction(&mut transaction_accounts, vault, deposit),
    ];
    let instructions = vec![execute_squads_program_interaction_instruction(
        policy,
        delegated_signer,
        vault_index,
        compiled_instructions,
        vec![
            plan.route.withdraw_constraint_index,
            plan.route.swap_constraint_index,
            plan.route.deposit_constraint_index,
        ],
        transaction_accounts,
    )];

    let preflight_accounts = required_token_accounts(vault, &plan.source, &plan.target)?;

    Ok(YieldRouteTransaction {
        report: json!({
            "kind": "cross_mint_route_transaction",
            "policy": policy.to_string(),
            "swapPolicy": swap_policy.map(|policy| policy.to_string()),
            "vault": vault.to_string(),
            "vaultIndex": vault_index,
            "delegatedSigner": delegated_signer.to_string(),
            "withdrawConstraintIndex": plan.route.withdraw_constraint_index,
            "swapConstraintIndex": plan.route.swap_constraint_index,
            "depositConstraintIndex": plan.route.deposit_constraint_index,
            "quote": plan.quote,
            "preflightAccounts": preflight_accounts_json(&preflight_accounts),
        }),
        preflight_accounts,
        instructions,
    })
}

pub fn build_kamino_deposit_sync_transaction(
    settings: Pubkey,
    signer: Pubkey,
    vault_index: u8,
    vault: Pubkey,
    target: &SameMintReserveTarget,
    amount: u64,
) -> Result<KaminoDepositSyncTransaction, RouteBuildError> {
    let deposit =
        kamino_deposit_reserve_liquidity_instruction(deposit_accounts(vault, target)?, amount);

    let mut transaction_accounts = Vec::new();
    let compiled_instructions = vec![compile_squads_vault_instruction(
        &mut transaction_accounts,
        vault,
        deposit,
    )];
    let instruction = execute_squads_sync_transaction_instruction(
        settings,
        signer,
        vault_index,
        compiled_instructions,
        transaction_accounts,
    );

    Ok(KaminoDepositSyncTransaction {
        instruction,
        report: json!({
            "kind": "kamino_deposit_sync_transaction",
            "settings": settings.to_string(),
            "vault": vault.to_string(),
            "vaultIndex": vault_index,
            "signer": signer.to_string(),
            "target": target,
            "amount": amount,
        }),
    })
}

pub fn associated_token_address(wallet: Pubkey, token_program: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[wallet.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn required_token_accounts(
    vault: Pubkey,
    source: &SameMintReserveTarget,
    target: &SameMintReserveTarget,
) -> Result<Vec<RoutePreflightAccount>, RouteBuildError> {
    let mut accounts = Vec::new();
    push_target_token_accounts(&mut accounts, vault, "source", source)?;
    push_target_token_accounts(&mut accounts, vault, "target", target)?;
    Ok(accounts)
}

fn push_target_token_accounts(
    accounts: &mut Vec<RoutePreflightAccount>,
    vault: Pubkey,
    label_prefix: &'static str,
    target: &SameMintReserveTarget,
) -> Result<(), RouteBuildError> {
    let token_program = token_program(&target.accounts)?;
    let liquidity_mint = parse_pubkey(
        if label_prefix == "source" {
            "source.liquidity_mint"
        } else {
            "target.liquidity_mint"
        },
        &target.liquidity_mint,
    )?;
    let collateral_mint = parse_pubkey(
        if label_prefix == "source" {
            "source.accounts.reserve_collateral_mint"
        } else {
            "target.accounts.reserve_collateral_mint"
        },
        &target.accounts.reserve_collateral_mint,
    )?;
    push_preflight_account(
        accounts,
        format!("{label_prefix}_liquidity_ata"),
        associated_token_address(vault, token_program, liquidity_mint),
        token_program,
    );
    push_preflight_account(
        accounts,
        format!("{label_prefix}_collateral_ata"),
        associated_token_address(vault, token_program, collateral_mint),
        token_program,
    );
    Ok(())
}

fn push_preflight_account(
    accounts: &mut Vec<RoutePreflightAccount>,
    label: String,
    address: Pubkey,
    owner_program: Pubkey,
) {
    if accounts.iter().any(|account| account.address == address) {
        return;
    }
    accounts.push(RoutePreflightAccount {
        label,
        address,
        owner_program,
    });
}

fn preflight_accounts_json(accounts: &[RoutePreflightAccount]) -> Value {
    Value::Array(
        accounts
            .iter()
            .map(|account| {
                json!({
                    "label": account.label,
                    "address": account.address.to_string(),
                    "ownerProgram": account.owner_program.to_string(),
                })
            })
            .collect(),
    )
}

fn swap_instruction(config: &RouteInstructionConfig) -> Result<Instruction, RouteBuildError> {
    Ok(Instruction {
        program_id: parse_pubkey("quote.swap.instruction.program_id", &config.program_id)?,
        accounts: config
            .accounts
            .iter()
            .map(|account| {
                let pubkey =
                    parse_pubkey("quote.swap.instruction.accounts.pubkey", &account.pubkey)?;
                Ok(if account.is_writable {
                    AccountMeta::new(pubkey, account.is_signer)
                } else {
                    AccountMeta::new_readonly(pubkey, account.is_signer)
                })
            })
            .collect::<Result<Vec<_>, RouteBuildError>>()?,
        data: config.data.clone(),
    })
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
    let lending_market = parse_pubkey("source.market", &target.market)?;
    Ok(KaminoWithdrawReserveLiquidityAccounts {
        owner: vault,
        lending_market,
        reserve: parse_pubkey("source.reserve", &target.reserve)?,
        lending_market_authority: parse_lending_market_authority(
            "source.accounts.lending_market_authority",
            &target.accounts.lending_market_authority,
            lending_market,
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
    let lending_market = parse_pubkey("target.market", &target.market)?;
    Ok(KaminoDepositReserveLiquidityAccounts {
        owner: vault,
        reserve: parse_pubkey("target.reserve", &target.reserve)?,
        lending_market,
        lending_market_authority: parse_lending_market_authority(
            "target.accounts.lending_market_authority",
            &target.accounts.lending_market_authority,
            lending_market,
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

fn execute_squads_sync_transaction_instruction(
    settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(settings, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_transaction_args(
            account_index,
            squads_compiled_instruction_payload(&compiled_instructions),
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

fn serialize_squads_sync_transaction_args(account_index: u8, payload: Vec<u8>) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    account_index
        .serialize(&mut data)
        .expect("serialize Squads account index");
    SQUADS_SYNC_SIGNER_COUNT
        .serialize(&mut data)
        .expect("serialize Squads signer count");
    0u8.serialize(&mut data)
        .expect("serialize Squads transaction payload variant");
    payload
        .serialize(&mut data)
        .expect("serialize Squads transaction payload");
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

fn compile_squads_vault_instruction(
    transaction_accounts: &mut Vec<AccountMeta>,
    vault: Pubkey,
    instruction: Instruction,
) -> SquadsCompiledInstruction {
    let instruction = Instruction {
        accounts: instruction
            .accounts
            .into_iter()
            .map(|mut account| {
                if account.pubkey == vault {
                    account.is_signer = false;
                }
                account
            })
            .collect(),
        ..instruction
    };
    compile_inner_instruction(transaction_accounts, instruction)
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

fn parse_lending_market_authority(
    field: &'static str,
    value: &str,
    lending_market: Pubkey,
) -> Result<Pubkey, RouteBuildError> {
    if value.is_empty() {
        return Ok(Pubkey::find_program_address(
            &[b"lma", lending_market.as_ref()],
            &KAMINO_LENDING_PROGRAM_ID,
        )
        .0);
    }
    parse_pubkey(field, value)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::planner::{
        CrossMintQuote, RouteAccountMetaConfig, RouteInstructionConfig, SwapQuote,
    };

    #[test]
    fn builds_cross_mint_route_with_policy_constraint_indexes() {
        let policy = Pubkey::new_unique();
        let vault = Pubkey::new_unique();
        let delegated_signer = Pubkey::new_unique();
        let source = target(Pubkey::new_unique(), Pubkey::new_unique());
        let target = target(Pubkey::new_unique(), Pubkey::new_unique());
        let swap_program = Pubkey::new_unique();
        let plan = json!({
            "kind": "cross_mint",
            "route": {
                "policy_account": policy.to_string(),
                "vault_pubkey": vault.to_string(),
                "vault_index": 2,
                "withdraw_constraint_index": 0,
                "swap_constraint_index": 1,
                "deposit_constraint_index": 2
            },
            "quote": CrossMintQuote {
                redeem_collateral_amount: 1_000,
                redeem_liquidity_amount: 1_000,
                swap: SwapQuote {
                    lane_kind: "jupiter".to_owned(),
                    lane_index: 0,
                    source_mint: source.liquidity_mint.clone(),
                    target_mint: target.liquidity_mint.clone(),
                    amount_in: 1_000,
                    min_out: 990,
                    max_slippage_bps: Some(100),
                    max_fee_bps: None,
                    instruction: Some(RouteInstructionConfig {
                        program_id: swap_program.to_string(),
                        accounts: vec![
                            RouteAccountMetaConfig {
                                pubkey: vault.to_string(),
                                is_signer: true,
                                is_writable: false,
                            },
                        ],
                        data: vec![1, 2, 3],
                    }),
                },
                deposit_liquidity_amount: 990,
                expected_collateral_amount: 990,
            },
            "source": source,
            "target": target
        });

        let transaction = build_yield_route_transaction(&plan, delegated_signer).unwrap();

        assert_eq!(transaction.report["kind"], "cross_mint_route_transaction");
        assert_eq!(transaction.report["withdrawConstraintIndex"], 0);
        assert_eq!(transaction.report["swapConstraintIndex"], 1);
        assert_eq!(transaction.report["depositConstraintIndex"], 2);
        assert_eq!(transaction.instructions.len(), 1);
        assert_eq!(transaction.instructions[0].accounts[0].pubkey, policy);
        assert_eq!(transaction.preflight_accounts.len(), 4);
    }

    #[test]
    fn rejects_split_cross_mint_route_with_swap_policy() {
        let policy = Pubkey::new_unique();
        let swap_policy = Pubkey::new_unique();
        let vault = Pubkey::new_unique();
        let delegated_signer = Pubkey::new_unique();
        let source = target(Pubkey::new_unique(), Pubkey::new_unique());
        let target = target(Pubkey::new_unique(), Pubkey::new_unique());
        let swap_program = Pubkey::new_unique();
        let plan = json!({
            "kind": "cross_mint",
            "route": {
                "policy_account": policy.to_string(),
                "swap_policy_account": swap_policy.to_string(),
                "vault_pubkey": vault.to_string(),
                "vault_index": 2,
                "withdraw_constraint_index": 0,
                "swap_constraint_index": 0,
                "deposit_constraint_index": 1
            },
            "quote": CrossMintQuote {
                redeem_collateral_amount: 1_000,
                redeem_liquidity_amount: 1_000,
                swap: SwapQuote {
                    lane_kind: "jupiter".to_owned(),
                    lane_index: 0,
                    source_mint: source.liquidity_mint.clone(),
                    target_mint: target.liquidity_mint.clone(),
                    amount_in: 1_000,
                    min_out: 990,
                    max_slippage_bps: Some(100),
                    max_fee_bps: None,
                    instruction: Some(RouteInstructionConfig {
                        program_id: swap_program.to_string(),
                        accounts: vec![
                            RouteAccountMetaConfig {
                                pubkey: vault.to_string(),
                                is_signer: true,
                                is_writable: false,
                            },
                        ],
                        data: vec![1, 2, 3],
                    }),
                },
                deposit_liquidity_amount: 990,
                expected_collateral_amount: 990,
            },
            "source": source,
            "target": target
        });

        let error = build_yield_route_transaction(&plan, delegated_signer).unwrap_err();

        assert!(matches!(error, RouteBuildError::SplitSwapPolicyUnsupported));
    }

    fn target(reserve: Pubkey, mint: Pubkey) -> SameMintReserveTarget {
        SameMintReserveTarget {
            reserve: reserve.to_string(),
            market: Pubkey::new_unique().to_string(),
            liquidity_mint: mint.to_string(),
            supply_apy_bps: 100,
            accounts: KaminoReserveAccountsConfig {
                lending_market_authority: String::new(),
                reserve_liquidity_supply: Pubkey::new_unique().to_string(),
                reserve_collateral_mint: Pubkey::new_unique().to_string(),
                liquidity_token_program: Some(spl_token::ID.to_string()),
            },
            metadata: json!({}),
        }
    }
}
