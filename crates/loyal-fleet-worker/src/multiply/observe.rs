use super::config::{
    EarnMaxTopology, StrategyConfig, KLEND, PYUSD_MINT, TOKEN, TOKEN_2022, USDC_MINT,
};
use klend_interface::{
    from_account_data,
    state::{Obligation, Reserve},
};
use loyal_kamino_codec::{snapshot_from_account, ReserveTarget};
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
    pub obligation_last_update_slot: u64,
    pub collateral_reserve_last_update_slot: u64,
    pub debt_reserve_last_update_slot: u64,
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
    pub collateral_supply_apy_bps: u64,
    pub debt_borrow_apy_bps: u64,
}

#[derive(Clone, Debug)]
pub struct ObservedRoute {
    pub slot: u64,
    pub claim: TokenBalance,
    pub collateral_custody: TokenBalance,
    pub debt_custodies: Vec<(StrategyKey, TokenBalance)>,
    pub strategies: Vec<StrategyObservation>,
    pub external_custody: Vec<TokenBalance>,
}

impl ObservedRoute {
    pub fn position(&self, key: StrategyKey) -> &StrategyObservation {
        self.strategies
            .iter()
            .find(|value| value.strategy_key == key)
            .expect("the configured strategy is always observed")
    }

    pub fn debt_custody(&self, key: StrategyKey) -> &TokenBalance {
        &self
            .debt_custodies
            .iter()
            .find(|(strategy_key, _)| *strategy_key == key)
            .expect("the configured debt custody is always observed")
            .1
    }

    pub fn active_strategy_is_coherent(&self) -> bool {
        let active = self
            .strategies
            .iter()
            .filter(|position| position.collateral_deposited_raw > 0 || position.debt_raw > 0)
            .collect::<Vec<_>>();
        active.len() <= 1
            && active.first().is_none_or(|position| {
                position.collateral_reserve_last_update_slot >= position.obligation_last_update_slot
                    && position.debt_reserve_last_update_slot
                        >= position.obligation_last_update_slot
            })
    }
}

pub async fn observe_confirmed(
    rpc: &RpcClient,
    topology: EarnMaxTopology,
) -> Result<ObservedRoute, Box<dyn Error>> {
    observe_confirmed_with_extra(rpc, topology, &[]).await
}

pub async fn observe_confirmed_with_extra(
    rpc: &RpcClient,
    topology: EarnMaxTopology,
    extra: &[(&str, &str, &str)],
) -> Result<ObservedRoute, Box<dyn Error>> {
    let usdc_config = topology.strategy(StrategyKey::SyrupUsdcUsdc);
    let pyusd_config = topology.strategy(StrategyKey::SyrupUsdcPyusd);
    let keys = vec![
        topology.claim_custody,
        topology.collateral_custody,
        usdc_config.obligation,
        Pubkey::from_str(usdc_config.collateral_reserve)?,
        Pubkey::from_str(usdc_config.debt_reserve)?,
        pyusd_config.debt_custody,
        pyusd_config.obligation,
        Pubkey::from_str(pyusd_config.debt_reserve)?,
    ];
    let response = rpc
        .get_multiple_accounts_with_commitment(&keys, CommitmentConfig::confirmed())
        .await?;
    let account = |index: usize| {
        response.value[index]
            .as_ref()
            .ok_or_else(|| format!("required mainnet account {} is absent", keys[index]))
    };
    let usdc = classic_balance(account(0)?, USDC_MINT, topology.vault)?;
    let syrup = classic_balance(account(1)?, super::config::SYRUP_MINT, topology.vault)?;
    let collateral_reserve = decode_reserve(account(3)?, usdc_config)?;
    let usdc_debt_reserve = decode_reserve(account(4)?, usdc_config)?;
    let pyusd_debt_reserve = decode_reserve(account(7)?, pyusd_config)?;
    let collateral_supply_apy_bps = reserve_apy_bps(
        account(3)?,
        response.context.slot,
        Pubkey::from_str(usdc_config.collateral_reserve)?,
        true,
    )?;
    let usdc_debt_borrow_apy_bps = reserve_apy_bps(
        account(4)?,
        response.context.slot,
        Pubkey::from_str(usdc_config.debt_reserve)?,
        false,
    )?;
    let pyusd_debt_borrow_apy_bps = reserve_apy_bps(
        account(7)?,
        response.context.slot,
        Pubkey::from_str(pyusd_config.debt_reserve)?,
        false,
    )?;
    let usdc_strategy = match response.value[2].as_ref() {
        Some(value) => decode_obligation(
            value,
            usdc_config,
            topology.vault,
            &collateral_reserve,
            &usdc_debt_reserve,
            collateral_supply_apy_bps,
            usdc_debt_borrow_apy_bps,
        )?,
        None => empty_obligation(
            usdc_config,
            &collateral_reserve,
            &usdc_debt_reserve,
            collateral_supply_apy_bps,
            usdc_debt_borrow_apy_bps,
        )?,
    };
    let pyusd_strategy = match response.value[6].as_ref() {
        Some(value) => decode_obligation(
            value,
            pyusd_config,
            topology.vault,
            &collateral_reserve,
            &pyusd_debt_reserve,
            collateral_supply_apy_bps,
            pyusd_debt_borrow_apy_bps,
        )?,
        None => empty_obligation(
            pyusd_config,
            &collateral_reserve,
            &pyusd_debt_reserve,
            collateral_supply_apy_bps,
            pyusd_debt_borrow_apy_bps,
        )?,
    };
    let pyusd = response.value[5]
        .as_ref()
        .map(|value| token_2022_balance_for_owner(value, PYUSD_MINT, Some(topology.vault)))
        .transpose()?
        .unwrap_or(0);
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
        claim: token_balance(&topology.claim_custody.to_string(), USDC_MINT, TOKEN, usdc),
        collateral_custody: token_balance(
            &topology.collateral_custody.to_string(),
            super::config::SYRUP_MINT,
            TOKEN,
            syrup,
        ),
        debt_custodies: vec![
            (
                StrategyKey::SyrupUsdcUsdc,
                token_balance(
                    &usdc_config.debt_custody.to_string(),
                    USDC_MINT,
                    TOKEN,
                    usdc,
                ),
            ),
            (
                StrategyKey::SyrupUsdcPyusd,
                token_balance(
                    &pyusd_config.debt_custody.to_string(),
                    PYUSD_MINT,
                    TOKEN_2022,
                    pyusd,
                ),
            ),
        ],
        strategies: vec![usdc_strategy, pyusd_strategy],
        external_custody,
    })
}

fn classic_balance(
    account: &solana_sdk::account::Account,
    mint: &str,
    owner: Pubkey,
) -> Result<u64, Box<dyn Error>> {
    classic_balance_for_owner(account, mint, Some(owner))
}

fn classic_balance_for_owner(
    account: &solana_sdk::account::Account,
    mint: &str,
    expected_owner: Option<Pubkey>,
) -> Result<u64, Box<dyn Error>> {
    if account.owner != spl_token::id() {
        return Err("classic token account has the wrong owner".into());
    }
    let token = spl_token::state::Account::unpack(&account.data)?;
    if token.mint != Pubkey::from_str(mint)?
        || expected_owner.is_some_and(|owner| token.owner != owner)
    {
        return Err("classic custody mint or authority drifted".into());
    }
    Ok(token.amount)
}

fn token_2022_balance_for_owner(
    account: &solana_sdk::account::Account,
    mint: &str,
    expected_owner: Option<Pubkey>,
) -> Result<u64, Box<dyn Error>> {
    if account.owner != spl_token_2022::id() {
        return Err("Token-2022 account has the wrong owner".into());
    }
    let token = StateWithExtensions::<spl_token_2022::state::Account>::unpack(&account.data)?;
    if token.base.mint != Pubkey::from_str(mint)?
        || expected_owner.is_some_and(|owner| token.base.owner != owner)
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
    vault: Pubkey,
    collateral_reserve: &Reserve,
    debt_reserve: &Reserve,
    collateral_supply_apy_bps: u64,
    debt_borrow_apy_bps: u64,
) -> Result<StrategyObservation, Box<dyn Error>> {
    if account.owner != Pubkey::from_str(KLEND)? {
        return Err("obligation has the wrong program owner".into());
    }
    let obligation = from_account_data::<Obligation>(&account.data)?;
    if obligation.owner != vault
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
    let collateral_deposited_raw = deposits.first().map_or(0, |value| value.deposited_amount);
    Ok(StrategyObservation {
        strategy_key: config.key,
        obligation_last_update_slot: obligation.last_update.slot,
        collateral_reserve_last_update_slot: collateral_reserve.last_update.slot,
        debt_reserve_last_update_slot: debt_reserve.last_update.slot,
        collateral_deposited_raw,
        debt_raw,
        debt_amount_sf: debt_sf.to_string(),
        collateral_value_sf: collateral_market_value_sf(
            collateral_reserve,
            collateral_deposited_raw,
        )?,
        debt_value_sf: debt_market_value_sf(debt_reserve, debt_sf)?,
        unhealthy_value_sf: u128::from(obligation.unhealthy_borrow_value_sf),
        debt_market_price_sf: debt_reserve.market_price(),
        debt_mint_factor: 10_u64
            .checked_pow(u32::try_from(debt_reserve.mint_decimals())?)
            .ok_or("debt mint factor overflow")?,
        collateral_total_supply_raw: collateral_reserve.collateral_total_supply(),
        collateral_total_liquidity_sf: reserve_total_liquidity_sf(collateral_reserve)?.to_string(),
        collateral_supply_apy_bps,
        debt_borrow_apy_bps,
    })
}

fn empty_obligation(
    config: StrategyConfig,
    collateral_reserve: &Reserve,
    debt_reserve: &Reserve,
    collateral_supply_apy_bps: u64,
    debt_borrow_apy_bps: u64,
) -> Result<StrategyObservation, Box<dyn Error>> {
    Ok(StrategyObservation {
        strategy_key: config.key,
        obligation_last_update_slot: 0,
        collateral_reserve_last_update_slot: collateral_reserve.last_update.slot,
        debt_reserve_last_update_slot: debt_reserve.last_update.slot,
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
        collateral_supply_apy_bps,
        debt_borrow_apy_bps,
    })
}

fn reserve_apy_bps(
    account: &solana_sdk::account::Account,
    slot: u64,
    reserve: Pubkey,
    supply: bool,
) -> Result<u64, Box<dyn Error>> {
    let snapshot = snapshot_from_account(
        &ReserveTarget {
            reserve,
            market: None,
            market_name: None,
            symbol: None,
            liquidity_mint: None,
            api_supply_apy: None,
            api_borrow_apy: None,
            api_total_supply_usd: None,
            api_total_borrow_usd: None,
        },
        slot,
        &account.data,
        500.0,
    )?;
    let ratio = if supply {
        snapshot.supply_apy
    } else {
        snapshot.borrow_apy
    };
    if !ratio.is_finite() || ratio < 0.0 || ratio > (u64::MAX as f64) / 10_000.0 {
        return Err("reserve APY is outside the supported range".into());
    }
    Ok((ratio * 10_000.0).round() as u64)
}

fn decode_reserve(
    account: &solana_sdk::account::Account,
    config: StrategyConfig,
) -> Result<Reserve, Box<dyn Error>> {
    if account.owner != Pubkey::from_str(KLEND)? {
        return Err("reserve has the wrong program owner".into());
    }
    let reserve = *from_account_data::<Reserve>(&account.data)?;
    if reserve.lending_market != Pubkey::from_str(config.market)?
        || reserve.status() != 0
        || reserve.market_price() == 0
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

fn collateral_market_value_sf(
    reserve: &Reserve,
    collateral_raw: u64,
) -> Result<u128, Box<dyn Error>> {
    if collateral_raw == 0 {
        return Ok(0);
    }
    let denominator = BigUint::from(reserve.collateral_total_supply()) << 60_usize;
    if denominator.is_zero() {
        return Err("collateral reserve supply is zero".into());
    }
    let liquidity_raw =
        BigUint::from(collateral_raw) * reserve_total_liquidity_sf(reserve)? / denominator;
    let mint_factor = 10_u64
        .checked_pow(u32::try_from(reserve.mint_decimals())?)
        .ok_or("collateral mint factor overflow")?;
    (liquidity_raw * BigUint::from(reserve.market_price()) / BigUint::from(mint_factor))
        .to_u128()
        .ok_or_else(|| "collateral market value exceeds u128".into())
}

fn debt_market_value_sf(reserve: &Reserve, debt_amount_sf: u128) -> Result<u128, Box<dyn Error>> {
    if debt_amount_sf == 0 {
        return Ok(0);
    }
    let mint_factor = 10_u64
        .checked_pow(u32::try_from(reserve.mint_decimals())?)
        .ok_or("debt mint factor overflow")?;
    let denominator = BigUint::from(mint_factor) << 60_usize;
    (BigUint::from(debt_amount_sf) * BigUint::from(reserve.market_price()) / denominator)
        .to_u128()
        .ok_or_else(|| "debt market value exceeds u128".into())
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
    topology: EarnMaxTopology,
) -> loyal_yield_store::fleet_orchestration::MultiplyPosition {
    let config = topology.strategy(key);
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
        obligation: config.obligation.to_string(),
        collateral: TokenBalance {
            account: config.collateral_custody.to_string(),
            mint: config.collateral_mint.to_owned(),
            token_program: TOKEN.to_owned(),
            amount_raw: position.collateral_deposited_raw,
        },
        debt: TokenBalance {
            account: config.debt_custody.to_string(),
            mint: config.debt_mint.to_owned(),
            token_program: config.debt_token_program.to_owned(),
            amount_raw: position.debt_raw,
        },
        debt_amount_sf: position.debt_amount_sf.clone(),
        health_factor_ppm,
    }
}
