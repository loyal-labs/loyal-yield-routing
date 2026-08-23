use super::config::{
    strategy, StrategyConfig, KLEND, STRATEGIES, TOKEN, TOKEN_2022, USDC_CUSTODY, USDC_MINT, VAULT,
};
use klend_interface::{
    from_account_data,
    state::{Obligation, Reserve},
};
use loyal_yield_store::fleet_orchestration::{StrategyKey, TokenBalance};
use num_bigint::BigUint;
use num_traits::{ToPrimitive, Zero};
use solana_client::nonblocking::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, program_pack::Pack, pubkey::Pubkey};
use spl_token_2022::extension::StateWithExtensions;
use std::{error::Error, str::FromStr};

#[derive(Clone, Debug)]
pub struct StrategyObservation {
    pub strategy_key: StrategyKey,
    pub collateral_deposited_raw: u64,
    pub debt_raw: u64,
    pub debt_amount_sf: String,
    pub collateral_value_sf: u128,
    pub debt_value_sf: u128,
    pub unhealthy_value_sf: u128,
    pub debt_market_price_sf: u128,
    pub debt_mint_factor: u64,
    pub collateral_total_supply_raw: u64,
    pub collateral_total_liquidity_sf: String,
}

#[derive(Clone, Debug)]
pub struct ObservedRoute {
    pub slot: u64,
    pub claim: TokenBalance,
    pub collateral_custody: TokenBalance,
    pub source_debt_custody: TokenBalance,
    pub target_debt_custody: TokenBalance,
    pub strategies: Vec<StrategyObservation>,
    pub external_custody: Vec<TokenBalance>,
}

impl ObservedRoute {
    pub fn position(&self, key: StrategyKey) -> &StrategyObservation {
        self.strategies
            .iter()
            .find(|value| value.strategy_key == key)
            .expect("both configured strategies are always observed")
    }

    pub fn debt_custody(&self, key: StrategyKey) -> &TokenBalance {
        match key {
            StrategyKey::SyrupUsdcUsdc => &self.source_debt_custody,
            StrategyKey::SyrupUsdcPyusd => &self.target_debt_custody,
        }
    }
}

pub async fn observe_confirmed(rpc: &RpcClient) -> Result<ObservedRoute, Box<dyn Error>> {
    observe_confirmed_with_extra(rpc, &[]).await
}

pub async fn observe_confirmed_with_extra(
    rpc: &RpcClient,
    extra: &[(&str, &str, &str)],
) -> Result<ObservedRoute, Box<dyn Error>> {
    let keys = [
        USDC_CUSTODY,
        super::config::SYRUP_CUSTODY,
        super::config::PYUSD_CUSTODY,
        STRATEGIES[0].obligation,
        STRATEGIES[1].obligation,
        STRATEGIES[0].collateral_reserve,
        STRATEGIES[0].debt_reserve,
        STRATEGIES[1].debt_reserve,
    ]
    .map(Pubkey::from_str)
    .into_iter()
    .collect::<Result<Vec<_>, _>>()?;
    let response = rpc
        .get_multiple_accounts_with_commitment(&keys, CommitmentConfig::confirmed())
        .await?;
    let account = |index: usize| {
        response.value[index]
            .as_ref()
            .ok_or_else(|| format!("required mainnet account {} is absent", keys[index]))
    };
    let usdc = classic_balance(account(0)?, USDC_MINT)?;
    let syrup = classic_balance(account(1)?, super::config::SYRUP_MINT)?;
    let pyusd = token_2022_balance(account(2)?, super::config::PYUSD_MINT)?;
    let collateral_reserve = decode_reserve(account(5)?, STRATEGIES[0].collateral_reserve)?;
    let source_debt_reserve = decode_reserve(account(6)?, STRATEGIES[0].debt_reserve)?;
    let target_debt_reserve = decode_reserve(account(7)?, STRATEGIES[1].debt_reserve)?;
    let source = match response.value[3].as_ref() {
        Some(value) => decode_obligation(
            value,
            STRATEGIES[0],
            &collateral_reserve,
            &source_debt_reserve,
        )?,
        None => empty_obligation(STRATEGIES[0], &collateral_reserve, &source_debt_reserve)?,
    };
    let target = match response.value[4].as_ref() {
        Some(value) => decode_obligation(
            value,
            STRATEGIES[1],
            &collateral_reserve,
            &target_debt_reserve,
        )?,
        None => empty_obligation(STRATEGIES[1], &collateral_reserve, &target_debt_reserve)?,
    };
    let mut external_custody = Vec::with_capacity(extra.len());
    for (account_key, mint, token_program) in extra {
        let value = rpc
            .get_account_with_commitment(
                &Pubkey::from_str(account_key)?,
                CommitmentConfig::confirmed(),
            )
            .await?
            .value
            .ok_or("external custody account is absent")?;
        let amount_raw = if *token_program == TOKEN {
            classic_balance_for_owner(&value, mint, None)?
        } else if *token_program == TOKEN_2022 {
            token_2022_balance_for_owner(&value, mint, None)?
        } else {
            return Err("external custody token program is unsupported".into());
        };
        external_custody.push(token_balance(account_key, mint, token_program, amount_raw));
    }
    Ok(ObservedRoute {
        slot: response.context.slot,
        claim: token_balance(USDC_CUSTODY, USDC_MINT, TOKEN, usdc),
        collateral_custody: token_balance(
            super::config::SYRUP_CUSTODY,
            super::config::SYRUP_MINT,
            TOKEN,
            syrup,
        ),
        source_debt_custody: token_balance(USDC_CUSTODY, USDC_MINT, TOKEN, usdc),
        target_debt_custody: token_balance(
            super::config::PYUSD_CUSTODY,
            super::config::PYUSD_MINT,
            TOKEN_2022,
            pyusd,
        ),
        strategies: vec![source, target],
        external_custody,
    })
}

fn classic_balance(
    account: &solana_sdk::account::Account,
    mint: &str,
) -> Result<u64, Box<dyn Error>> {
    classic_balance_for_owner(account, mint, Some(VAULT))
}

fn classic_balance_for_owner(
    account: &solana_sdk::account::Account,
    mint: &str,
    expected_owner: Option<&str>,
) -> Result<u64, Box<dyn Error>> {
    if account.owner != spl_token::id() {
        return Err("classic token account has the wrong owner".into());
    }
    let token = spl_token::state::Account::unpack(&account.data)?;
    if token.mint != Pubkey::from_str(mint)?
        || expected_owner
            .map(Pubkey::from_str)
            .transpose()?
            .is_some_and(|owner| token.owner != owner)
    {
        return Err("classic custody mint or authority drifted".into());
    }
    Ok(token.amount)
}

fn token_2022_balance(
    account: &solana_sdk::account::Account,
    mint: &str,
) -> Result<u64, Box<dyn Error>> {
    token_2022_balance_for_owner(account, mint, Some(VAULT))
}

fn token_2022_balance_for_owner(
    account: &solana_sdk::account::Account,
    mint: &str,
    expected_owner: Option<&str>,
) -> Result<u64, Box<dyn Error>> {
    if account.owner != spl_token_2022::id() {
        return Err("Token-2022 account has the wrong owner".into());
    }
    let token = StateWithExtensions::<spl_token_2022::state::Account>::unpack(&account.data)?;
    if token.base.mint != Pubkey::from_str(mint)?
        || expected_owner
            .map(Pubkey::from_str)
            .transpose()?
            .is_some_and(|owner| token.base.owner != owner)
    {
        return Err("Token-2022 custody mint or authority drifted".into());
    }
    Ok(token.base.amount)
}

fn token_balance(account: &str, mint: &str, token_program: &str, amount_raw: u64) -> TokenBalance {
    TokenBalance {
        account: account.to_owned(),
        mint: mint.to_owned(),
        token_program: token_program.to_owned(),
        amount_raw,
    }
}

fn decode_obligation(
    account: &solana_sdk::account::Account,
    config: StrategyConfig,
    collateral_reserve: &Reserve,
    debt_reserve: &Reserve,
) -> Result<StrategyObservation, Box<dyn Error>> {
    if account.owner != Pubkey::from_str(KLEND)? {
        return Err("obligation has the wrong program owner".into());
    }
    let obligation = from_account_data::<Obligation>(&account.data)?;
    if obligation.owner != Pubkey::from_str(VAULT)?
        || obligation.lending_market != Pubkey::from_str(config.market)?
        || obligation.elevation_group != 0
    {
        return Err("obligation identity drifted".into());
    }
    let deposits = obligation
        .deposits
        .iter()
        .filter(|value| value.deposit_reserve != Pubkey::default())
        .collect::<Vec<_>>();
    let borrows = obligation
        .borrows
        .iter()
        .filter(|value| value.borrow_reserve != Pubkey::default())
        .collect::<Vec<_>>();
    if deposits.len() > 1
        || borrows.len() > 1
        || deposits.first().is_some_and(|value| {
            value.deposit_reserve
                != Pubkey::from_str(config.collateral_reserve).expect("static key")
        })
        || borrows.first().is_some_and(|value| {
            value.borrow_reserve != Pubkey::from_str(config.debt_reserve).expect("static key")
        })
    {
        return Err("obligation reserve topology drifted".into());
    }
    let debt_sf = borrows
        .first()
        .map_or(0, |value| u128::from(value.borrowed_amount_sf));
    let debt_raw = debt_sf
        .saturating_add((1_u128 << 60) - 1)
        .checked_shr(60)
        .and_then(|value| u64::try_from(value).ok())
        .ok_or("obligation debt exceeds u64")?;
    Ok(StrategyObservation {
        strategy_key: config.key,
        collateral_deposited_raw: deposits.first().map_or(0, |value| value.deposited_amount),
        debt_raw,
        debt_amount_sf: debt_sf.to_string(),
        collateral_value_sf: u128::from(obligation.deposited_value_sf),
        debt_value_sf: u128::from(obligation.borrowed_assets_market_value_sf),
        unhealthy_value_sf: u128::from(obligation.unhealthy_borrow_value_sf),
        debt_market_price_sf: debt_reserve.market_price(),
        debt_mint_factor: 10_u64
            .checked_pow(u32::try_from(debt_reserve.mint_decimals())?)
            .ok_or("debt mint factor overflow")?,
        collateral_total_supply_raw: collateral_reserve.collateral_total_supply(),
        collateral_total_liquidity_sf: reserve_total_liquidity_sf(collateral_reserve)?.to_string(),
    })
}

fn empty_obligation(
    config: StrategyConfig,
    collateral_reserve: &Reserve,
    debt_reserve: &Reserve,
) -> Result<StrategyObservation, Box<dyn Error>> {
    Ok(StrategyObservation {
        strategy_key: config.key,
        collateral_deposited_raw: 0,
        debt_raw: 0,
        debt_amount_sf: "0".to_owned(),
        collateral_value_sf: 0,
        debt_value_sf: 0,
        unhealthy_value_sf: 0,
        debt_market_price_sf: debt_reserve.market_price(),
        debt_mint_factor: 10_u64
            .checked_pow(u32::try_from(debt_reserve.mint_decimals())?)
            .ok_or("debt mint factor overflow")?,
        collateral_total_supply_raw: collateral_reserve.collateral_total_supply(),
        collateral_total_liquidity_sf: reserve_total_liquidity_sf(collateral_reserve)?.to_string(),
    })
}

fn decode_reserve(
    account: &solana_sdk::account::Account,
    expected: &str,
) -> Result<Reserve, Box<dyn Error>> {
    if account.owner != Pubkey::from_str(KLEND)? {
        return Err("reserve has the wrong program owner".into());
    }
    let reserve = *from_account_data::<Reserve>(&account.data)?;
    if reserve.lending_market != Pubkey::from_str(STRATEGIES[0].market)?
        || reserve.status() != 0
        || reserve.market_price() == 0
        || Pubkey::from_str(expected)? == Pubkey::default()
    {
        return Err("reserve identity, status, or price drifted".into());
    }
    Ok(reserve)
}

fn reserve_total_liquidity_sf(reserve: &Reserve) -> Result<BigUint, Box<dyn Error>> {
    let mut total = BigUint::from(reserve.available_liquidity()) << 60_usize;
    total += BigUint::from(reserve.borrowed_amount());
    for fee in [
        reserve.accumulated_protocol_fees(),
        reserve.accumulated_referrer_fees(),
        u128::from(reserve.liquidity.pending_referrer_fees_sf),
    ] {
        let fee = BigUint::from(fee);
        if total < fee {
            return Err("reserve total liquidity underflowed fees".into());
        }
        total -= fee;
    }
    if total.is_zero() {
        return Err("reserve total liquidity is zero".into());
    }
    Ok(total)
}

pub fn collateral_to_liquidity_raw(
    position: &StrategyObservation,
    collateral_raw: u64,
) -> Result<u64, Box<dyn Error>> {
    if collateral_raw == 0 {
        return Ok(0);
    }
    let total_liquidity = position.collateral_total_liquidity_sf.parse::<BigUint>()?;
    let denominator = BigUint::from(position.collateral_total_supply_raw) << 60_usize;
    if denominator.is_zero() {
        return Err("collateral reserve supply is zero".into());
    }
    (BigUint::from(collateral_raw) * total_liquidity / denominator)
        .to_u64()
        .ok_or_else(|| "redeemable collateral exceeds u64".into())
}

pub fn position_balance(
    observation: &ObservedRoute,
    key: StrategyKey,
) -> loyal_yield_store::fleet_orchestration::MultiplyPosition {
    let config = strategy(key);
    let position = observation.position(key);
    let health_factor_ppm = if position.debt_value_sf == 0 {
        u64::MAX
    } else {
        u64::try_from(
            position.unhealthy_value_sf.saturating_mul(1_000_000) / position.debt_value_sf,
        )
        .unwrap_or(u64::MAX)
    };
    loyal_yield_store::fleet_orchestration::MultiplyPosition::Active {
        strategy_key: key,
        obligation: config.obligation.to_owned(),
        collateral: TokenBalance {
            account: config.collateral_custody.to_owned(),
            mint: config.collateral_mint.to_owned(),
            token_program: TOKEN.to_owned(),
            amount_raw: position.collateral_deposited_raw,
        },
        debt: TokenBalance {
            account: config.debt_custody.to_owned(),
            mint: config.debt_mint.to_owned(),
            token_program: config.debt_token_program.to_owned(),
            amount_raw: position.debt_raw,
        },
        debt_amount_sf: position.debt_amount_sf.clone(),
        health_factor_ppm,
    }
}
