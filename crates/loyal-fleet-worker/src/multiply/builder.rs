use super::{
    config::{EarnMaxTopology, StrategyConfig, JUPITER, TOKEN},
    observe::{collateral_to_liquidity_raw, ObservedRoute},
    planner::{ActionPlan, PlannedAmount},
};
use base64::{engine::general_purpose::STANDARD, Engine as _};
use klend_interface::instructions::{
    borrow_obligation_liquidity_v2, deposit_reserve_liquidity_and_obligation_collateral_v2,
    refresh_obligation as sdk_refresh_obligation, refresh_reserve as sdk_refresh_reserve,
    repay_obligation_liquidity_v2, withdraw_obligation_collateral_and_redeem_reserve_collateral_v2,
    BorrowObligationLiquidityV2Accounts, DepositReserveLiquidityAndObligationCollateralV2Accounts,
    RefreshObligationAccounts, RefreshReserveAccounts, RepayObligationLiquidityV2Accounts,
    WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts,
};
use loyal_yield_store::fleet_orchestration::{
    ExpectedEffects, MultiplyAction, ObligationDelta, TokenDelta,
};
use serde_json::{json, Value};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::{error::Error, str::FromStr, time::Duration};

const RESERVE_COLLATERAL_MINT: &str = "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu";

#[derive(Clone, Debug)]
pub struct BuiltOperation {
    pub instructions: Vec<Instruction>,
    pub lookup_tables: Vec<Pubkey>,
    pub expected_effects: ExpectedEffects,
    pub quote_context_slot: Option<u64>,
}

pub async fn build_operation(
    plan: &ActionPlan,
    observed: &ObservedRoute,
    topology: EarnMaxTopology,
) -> Result<BuiltOperation, Box<dyn Error>> {
    let config = plan.strategy_key.map(|key| topology.strategy(key));
    match plan.action {
        MultiplyAction::DepositCollateral => {
            let config = required_strategy(config)?;
            let amount = exact_amount(plan.amount)?;
            let position = observed.position(config.key);
            Ok(klend_operation(
                deposit(
                    config,
                    topology.vault,
                    amount,
                    position.collateral_deposited_raw > 0,
                    position.debt_raw > 0,
                )?,
                token_effect(
                    config.collateral_custody,
                    config.collateral_mint,
                    -(amount as i64),
                ),
                obligation_effect(config, amount as i64, 0),
            ))
        }
        MultiplyAction::BorrowDebt => {
            let config = required_strategy(config)?;
            let amount = resolve_borrow(plan.amount, observed, config)?;
            let position = observed.position(config.key);
            Ok(klend_operation(
                borrow(
                    config,
                    topology.vault,
                    amount,
                    position.collateral_deposited_raw > 0,
                    position.debt_raw > 0,
                )?,
                token_effect(config.debt_custody, config.debt_mint, amount as i64),
                obligation_effect(config, 0, amount as i64),
            ))
        }
        MultiplyAction::WithdrawCollateral | MultiplyAction::WithdrawRemainingCollateral => {
            let config = required_strategy(config)?;
            let position = observed.position(config.key);
            let (wire_collateral_amount, expected_collateral_amount) = match plan.amount {
                PlannedAmount::Exact(value) => (value, value),
                PlannedAmount::All => (
                    position.collateral_deposited_raw,
                    position.collateral_deposited_raw,
                ),
                PlannedAmount::MaxSafe => (u64::MAX, 1),
                PlannedAmount::ToTargetLtv => return Err("invalid withdraw amount mode".into()),
            };
            let liquidity_amount = if plan.amount == PlannedAmount::MaxSafe {
                1
            } else {
                collateral_to_liquidity_raw(position, expected_collateral_amount)?
            };
            if liquidity_amount == 0 {
                return Err("collateral withdrawal would redeem zero liquidity".into());
            }
            Ok(klend_operation(
                withdraw(
                    config,
                    topology.vault,
                    wire_collateral_amount,
                    position.debt_raw > 0,
                )?,
                token_effect(
                    config.collateral_custody,
                    config.collateral_mint,
                    liquidity_amount as i64,
                ),
                obligation_effect(config, -(expected_collateral_amount as i64), 0),
            ))
        }
        MultiplyAction::RepayDebt => {
            let config = required_strategy(config)?;
            let debt = observed.position(config.key).debt_raw;
            let (wire_amount, expected_amount) = match plan.amount {
                PlannedAmount::All => (u64::MAX, debt),
                PlannedAmount::Exact(value) if value > 0 && value < debt => (value, value),
                _ => return Err("repay requires an exact partial amount or repay-all".into()),
            };
            Ok(klend_operation(
                repay(config, topology.vault, wire_amount)?,
                token_effect(
                    config.debt_custody,
                    config.debt_mint,
                    -(expected_amount as i64),
                ),
                obligation_effect(config, 0, -(expected_amount as i64)),
            ))
        }
        MultiplyAction::SwapClaimToCollateral => {
            let config = required_strategy(config)?;
            let amount = exact_amount(plan.amount)?;
            swap_exact_in(
                super::config::USDC_MINT,
                config.collateral_mint,
                amount,
                topology.claim_custody,
                config.collateral_custody,
                topology.vault,
            )
            .await
        }
        MultiplyAction::SwapDebtToCollateral => {
            let config = required_strategy(config)?;
            let amount = exact_amount(plan.amount)?;
            swap_exact_in(
                config.debt_mint,
                config.collateral_mint,
                amount,
                config.debt_custody,
                config.collateral_custody,
                topology.vault,
            )
            .await
        }
        MultiplyAction::SwapCollateralToDebt => {
            let config = required_strategy(config)?;
            let amount = exact_amount(plan.amount)?;
            swap_exact_in(
                config.collateral_mint,
                config.debt_mint,
                amount,
                config.collateral_custody,
                config.debt_custody,
                topology.vault,
            )
            .await
        }
        MultiplyAction::SwapCollateralToClaim => {
            let config = required_strategy(config)?;
            let amount = exact_amount(plan.amount)?;
            swap_exact_in(
                config.collateral_mint,
                super::config::USDC_MINT,
                amount,
                config.collateral_custody,
                topology.claim_custody,
                topology.vault,
            )
            .await
        }
        MultiplyAction::Claim => {
            let amount = exact_amount(plan.amount)?;
            let destination = plan
                .destination_account
                .as_deref()
                .ok_or("claim omitted its request-bound destination")?;
            let instruction = spl_token::instruction::transfer_checked(
                &spl_token::id(),
                &topology.claim_custody,
                &Pubkey::from_str(super::config::USDC_MINT)?,
                &Pubkey::from_str(destination)?,
                &topology.vault,
                &[],
                amount,
                6,
            )?;
            Ok(BuiltOperation {
                instructions: vec![instruction],
                lookup_tables: Vec::new(),
                expected_effects: ExpectedEffects {
                    token_amounts_before: Vec::new(),
                    token_deltas: vec![
                        token_effect(
                            topology.claim_custody,
                            super::config::USDC_MINT,
                            -(amount as i64),
                        ),
                        token_effect(destination, super::config::USDC_MINT, amount as i64),
                    ],
                    obligation_before: None,
                    obligation_delta: None,
                },
                quote_context_slot: None,
            })
        }
        MultiplyAction::DepositClaimAsset => {
            Err("user deposits are admitted from their confirmed wallet transaction".into())
        }
        MultiplyAction::RequestWithdrawal | MultiplyAction::CancelWithdrawal => {
            Err("withdrawal intents are admitted from their confirmed wallet transaction".into())
        }
    }
}

fn klend_operation(
    instructions: Vec<Instruction>,
    token_delta: TokenDelta,
    obligation_delta: ObligationDelta,
) -> BuiltOperation {
    BuiltOperation {
        instructions,
        lookup_tables: Vec::new(),
        expected_effects: ExpectedEffects {
            token_amounts_before: Vec::new(),
            token_deltas: vec![token_delta],
            obligation_before: None,
            obligation_delta: Some(obligation_delta),
        },
        quote_context_slot: None,
    }
}

fn token_effect(account: impl ToString, mint: &str, raw_delta: i64) -> TokenDelta {
    TokenDelta {
        account: account.to_string(),
        mint: mint.to_owned(),
        raw_delta,
    }
}

fn obligation_effect(
    config: StrategyConfig,
    collateral_raw_delta: i64,
    debt_raw_delta: i64,
) -> ObligationDelta {
    ObligationDelta {
        obligation: config.obligation.to_string(),
        collateral_raw_delta,
        debt_raw_delta,
    }
}

fn required_strategy(config: Option<StrategyConfig>) -> Result<StrategyConfig, Box<dyn Error>> {
    config.ok_or_else(|| "operation requires a strategy".into())
}

fn exact_amount(amount: PlannedAmount) -> Result<u64, Box<dyn Error>> {
    match amount {
        PlannedAmount::Exact(value) if value > 0 => Ok(value),
        _ => Err("operation requires a positive exact amount".into()),
    }
}

pub(crate) fn policy_template(
    config: StrategyConfig,
    vault: Pubkey,
    action: MultiplyAction,
) -> Result<Instruction, Box<dyn Error>> {
    let mut instructions = match action {
        MultiplyAction::DepositCollateral => deposit(config, vault, 1, true, true)?,
        MultiplyAction::BorrowDebt => borrow(config, vault, 1, true, true)?,
        MultiplyAction::WithdrawCollateral => withdraw(config, vault, 1, true)?,
        MultiplyAction::RepayDebt => repay(config, vault, u64::MAX)?,
        _ => return Err("action has no KLend policy template".into()),
    };
    instructions
        .pop()
        .ok_or_else(|| "KLend policy template is empty".into())
}

fn resolve_borrow(
    amount: PlannedAmount,
    observed: &ObservedRoute,
    config: StrategyConfig,
) -> Result<u64, Box<dyn Error>> {
    if let PlannedAmount::Exact(value) = amount {
        return Ok(value);
    }
    if amount != PlannedAmount::ToTargetLtv {
        return Err("invalid borrow amount mode".into());
    }
    let position = observed.position(config.key);
    let target_value_sf = position
        .collateral_value_sf
        .saturating_mul(u128::from(config.target_ltv_bps))
        / 10_000;
    let additional_value_sf = target_value_sf.saturating_sub(position.debt_value_sf);
    if position.debt_market_price_sf == 0 {
        return Err("debt reserve price is zero".into());
    }
    let raw = additional_value_sf
        .saturating_mul(u128::from(position.debt_mint_factor))
        .checked_div(position.debt_market_price_sf)
        .and_then(|value| u64::try_from(value).ok())
        .ok_or("borrow amount exceeds u64")?;
    if raw == 0 {
        Err("position is already at target LTV".into())
    } else {
        Ok(raw)
    }
}

fn refresh(config: StrategyConfig, reserve: &str) -> Result<Instruction, Box<dyn Error>> {
    Ok(sdk_refresh_reserve(RefreshReserveAccounts {
        reserve: Pubkey::from_str(reserve)?,
        lending_market: Pubkey::from_str(config.market)?,
        pyth_oracle: None,
        switchboard_price_oracle: None,
        switchboard_twap_oracle: None,
        scope_prices: Some(Pubkey::from_str(config.oracle)?),
    }))
}

fn refresh_obligation(
    config: StrategyConfig,
    include_collateral: bool,
    include_debt: bool,
) -> Result<Instruction, Box<dyn Error>> {
    let mut accounts = Vec::new();
    if include_collateral {
        accounts.push(AccountMeta::new(
            Pubkey::from_str(config.collateral_reserve)?,
            false,
        ));
    }
    if include_debt {
        accounts.push(AccountMeta::new(
            Pubkey::from_str(config.debt_reserve)?,
            false,
        ));
    }
    Ok(sdk_refresh_obligation(
        RefreshObligationAccounts {
            lending_market: Pubkey::from_str(config.market)?,
            obligation: config.obligation,
        },
        accounts,
    ))
}

fn deposit(
    config: StrategyConfig,
    vault: Pubkey,
    amount: u64,
    include_collateral: bool,
    include_debt: bool,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    Ok(vec![
        refresh(config, config.collateral_reserve)?,
        refresh(config, config.debt_reserve)?,
        refresh_obligation(config, include_collateral, include_debt)?,
        deposit_reserve_liquidity_and_obligation_collateral_v2(
            DepositReserveLiquidityAndObligationCollateralV2Accounts {
                owner: vault,
                obligation: config.obligation,
                lending_market: Pubkey::from_str(config.market)?,
                lending_market_authority: Pubkey::from_str(config.market_authority)?,
                reserve: Pubkey::from_str(config.collateral_reserve)?,
                reserve_liquidity_mint: Pubkey::from_str(config.collateral_mint)?,
                reserve_liquidity_supply: Pubkey::from_str(config.collateral_liquidity_supply)?,
                reserve_collateral_mint: Pubkey::from_str(RESERVE_COLLATERAL_MINT)?,
                reserve_destination_deposit_collateral: Pubkey::from_str(
                    config.collateral_mint_supply,
                )?,
                user_source_liquidity: config.collateral_custody,
                placeholder_user_destination_collateral: None,
                liquidity_token_program: Pubkey::from_str(TOKEN)?,
                obligation_farm_user_state: config.collateral_farm_user,
                reserve_farm_state: config
                    .collateral_farm_state
                    .map(Pubkey::from_str)
                    .transpose()?,
            },
            amount,
        ),
    ])
}

fn borrow(
    config: StrategyConfig,
    vault: Pubkey,
    amount: u64,
    include_collateral: bool,
    include_debt: bool,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    Ok(vec![
        refresh(config, config.collateral_reserve)?,
        refresh(config, config.debt_reserve)?,
        refresh_obligation(config, include_collateral, include_debt)?,
        borrow_obligation_liquidity_v2(
            BorrowObligationLiquidityV2Accounts {
                owner: vault,
                obligation: config.obligation,
                lending_market: Pubkey::from_str(config.market)?,
                lending_market_authority: Pubkey::from_str(config.market_authority)?,
                borrow_reserve: Pubkey::from_str(config.debt_reserve)?,
                borrow_reserve_liquidity_mint: Pubkey::from_str(config.debt_mint)?,
                reserve_source_liquidity: Pubkey::from_str(config.debt_liquidity_supply)?,
                borrow_reserve_liquidity_fee_receiver: Pubkey::from_str(config.debt_fee_vault)?,
                user_destination_liquidity: config.debt_custody,
                referrer_token_state: None,
                token_program: Pubkey::from_str(config.debt_token_program)?,
                obligation_farm_user_state: config.debt_farm_user,
                reserve_farm_state: config.debt_farm_state.map(Pubkey::from_str).transpose()?,
            },
            amount,
            Vec::new(),
        ),
    ])
}

fn withdraw(
    config: StrategyConfig,
    vault: Pubkey,
    amount: u64,
    debt_aware: bool,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    let mut result = vec![refresh(config, config.collateral_reserve)?];
    if debt_aware {
        result.push(refresh(config, config.debt_reserve)?);
    }
    result.push(refresh_obligation(config, true, debt_aware)?);
    result.push(
        withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
                owner: vault,
                obligation: config.obligation,
                lending_market: Pubkey::from_str(config.market)?,
                lending_market_authority: Pubkey::from_str(config.market_authority)?,
                withdraw_reserve: Pubkey::from_str(config.collateral_reserve)?,
                reserve_liquidity_mint: Pubkey::from_str(config.collateral_mint)?,
                reserve_source_collateral: Pubkey::from_str(config.collateral_mint_supply)?,
                reserve_collateral_mint: Pubkey::from_str(RESERVE_COLLATERAL_MINT)?,
                reserve_liquidity_supply: Pubkey::from_str(config.collateral_liquidity_supply)?,
                user_destination_liquidity: config.collateral_custody,
                placeholder_user_destination_collateral: None,
                liquidity_token_program: Pubkey::from_str(TOKEN)?,
                obligation_farm_user_state: config.collateral_farm_user,
                reserve_farm_state: config
                    .collateral_farm_state
                    .map(Pubkey::from_str)
                    .transpose()?,
            },
            amount,
        ),
    );
    Ok(result)
}

fn repay(
    config: StrategyConfig,
    vault: Pubkey,
    amount: u64,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    Ok(vec![
        refresh(config, config.collateral_reserve)?,
        refresh(config, config.debt_reserve)?,
        refresh_obligation(config, true, true)?,
        repay_obligation_liquidity_v2(
            RepayObligationLiquidityV2Accounts {
                owner: vault,
                obligation: config.obligation,
                lending_market: Pubkey::from_str(config.market)?,
                repay_reserve: Pubkey::from_str(config.debt_reserve)?,
                reserve_liquidity_mint: Pubkey::from_str(config.debt_mint)?,
                reserve_destination_liquidity: Pubkey::from_str(config.debt_liquidity_supply)?,
                user_source_liquidity: config.debt_custody,
                token_program: Pubkey::from_str(config.debt_token_program)?,
                obligation_farm_user_state: config.debt_farm_user,
                reserve_farm_state: config.debt_farm_state.map(Pubkey::from_str).transpose()?,
                lending_market_authority: Pubkey::from_str(config.market_authority)?,
            },
            amount,
            Vec::new(),
        ),
    ])
}

#[derive(Debug)]
struct JupiterQuote {
    instruction: Instruction,
    lookup_tables: Vec<Pubkey>,
    input_raw: u64,
    threshold_raw: u64,
    context_slot: u64,
}

async fn swap_exact_in(
    input_mint: &str,
    output_mint: &str,
    amount: u64,
    source: Pubkey,
    destination: Pubkey,
    vault: Pubkey,
) -> Result<BuiltOperation, Box<dyn Error>> {
    let quote = quote(input_mint, output_mint, amount, source, destination, vault).await?;
    Ok(BuiltOperation {
        instructions: vec![quote.instruction],
        lookup_tables: quote.lookup_tables,
        expected_effects: ExpectedEffects {
            token_amounts_before: Vec::new(),
            token_deltas: vec![
                token_effect(source, input_mint, -(quote.input_raw as i64)),
                token_effect(destination, output_mint, quote.threshold_raw as i64),
            ],
            obligation_before: None,
            obligation_delta: None,
        },
        quote_context_slot: Some(quote.context_slot),
    })
}

async fn quote(
    input_mint: &str,
    output_mint: &str,
    amount: u64,
    source: Pubkey,
    destination: Pubkey,
    vault: Pubkey,
) -> Result<JupiterQuote, Box<dyn Error>> {
    if amount == 0 {
        return Err("Jupiter amount must be positive".into());
    }
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(15))
        .build()?;
    let quote: Value = client
        .get("https://lite-api.jup.ag/swap/v1/quote")
        .query(&[
            ("inputMint", input_mint),
            ("outputMint", output_mint),
            ("amount", &amount.to_string()),
            ("swapMode", "ExactIn"),
            ("slippageBps", "50"),
            ("maxAccounts", "32"),
        ])
        .send()
        .await?
        .error_for_status()?
        .json()
        .await?;
    let field = |name: &str| {
        quote
            .get(name)
            .and_then(Value::as_str)
            .ok_or_else(|| format!("quote omitted {name}"))
    };
    if field("inputMint")? != input_mint
        || field("outputMint")? != output_mint
        || field("swapMode")? != "ExactIn"
        || quote
            .get("platformFee")
            .is_some_and(|value| !value.is_null())
    {
        return Err("Jupiter quote identity drifted".into());
    }
    let route_plan = quote
        .get("routePlan")
        .and_then(Value::as_array)
        .ok_or("quote omitted route plan")?;
    if route_plan.is_empty() || route_plan.len() > 4 {
        return Err("Jupiter quote must contain one to four route legs".into());
    }
    let input_raw = field("inAmount")?.parse::<u64>()?;
    let output_raw = field("outAmount")?.parse::<u64>()?;
    let threshold_raw = field("otherAmountThreshold")?.parse::<u64>()?;
    if input_raw != amount || output_raw < threshold_raw {
        return Err("Jupiter quote amount or threshold drifted".into());
    }
    let context_slot = quote
        .get("contextSlot")
        .and_then(Value::as_u64)
        .ok_or("quote omitted context slot")?;
    let response: Value = client
        .post("https://lite-api.jup.ag/swap/v1/swap-instructions")
        .json(&json!({
            "quoteResponse": quote, "userPublicKey": vault.to_string(), "useSharedAccounts": true,
            "wrapAndUnwrapSol": false, "dynamicComputeUnitLimit": false,
        }))
        .send()
        .await?
        .error_for_status()?
        .json()
        .await?;
    for name in ["setupInstructions", "otherInstructions"] {
        if response
            .get(name)
            .is_some_and(|value| value.as_array().is_none_or(|items| !items.is_empty()))
        {
            return Err("Jupiter introduced extra instructions".into());
        }
    }
    for name in ["cleanupInstruction", "tokenLedgerInstruction"] {
        if response.get(name).is_some_and(|value| !value.is_null()) {
            return Err("Jupiter introduced an optional instruction".into());
        }
    }
    let swap = response
        .get("swapInstruction")
        .and_then(Value::as_object)
        .ok_or("swap instruction missing")?;
    if swap.get("programId").and_then(Value::as_str) != Some(JUPITER) {
        return Err("Jupiter program drifted".into());
    }
    let accounts = swap
        .get("accounts")
        .and_then(Value::as_array)
        .ok_or("swap accounts missing")?
        .iter()
        .map(|value| {
            let key = Pubkey::from_str(
                value
                    .get("pubkey")
                    .and_then(Value::as_str)
                    .ok_or("swap key missing")?,
            )?;
            let signer = value
                .get("isSigner")
                .and_then(Value::as_bool)
                .ok_or("swap signer flag missing")?;
            let writable = value
                .get("isWritable")
                .and_then(Value::as_bool)
                .ok_or("swap writable flag missing")?;
            Ok::<_, Box<dyn Error>>(if writable {
                AccountMeta::new(key, signer)
            } else {
                AccountMeta::new_readonly(key, signer)
            })
        })
        .collect::<Result<Vec<_>, _>>()?;
    if !accounts
        .iter()
        .any(|value| value.pubkey == source && value.is_writable)
        || !accounts
            .iter()
            .any(|value| value.pubkey == destination && value.is_writable)
        || !accounts
            .iter()
            .any(|value| value.pubkey == vault && value.is_signer)
    {
        return Err("Jupiter did not bind vault custody".into());
    }
    let lookup_tables = response
        .get("addressLookupTableAddresses")
        .and_then(Value::as_array)
        .ok_or("lookup tables missing")?
        .iter()
        .map(|value| {
            let value = value
                .as_str()
                .ok_or_else(|| -> Box<dyn Error> { "lookup table key is not text".into() })?;
            Pubkey::from_str(value).map_err(|error| -> Box<dyn Error> { Box::new(error) })
        })
        .collect::<Result<Vec<_>, _>>()?;
    Ok(JupiterQuote {
        instruction: Instruction {
            program_id: Pubkey::from_str(JUPITER)?,
            accounts,
            data: STANDARD.decode(
                swap.get("data")
                    .and_then(Value::as_str)
                    .ok_or("swap data missing")?,
            )?,
        },
        lookup_tables,
        input_raw,
        threshold_raw,
        context_slot,
    })
}
