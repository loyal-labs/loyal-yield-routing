use klend_interface::{
    instructions::{
        deposit::{
            deposit_reserve_liquidity_and_obligation_collateral_v2,
            DepositReserveLiquidityAndObligationCollateralV2Accounts,
        },
        refresh::{
            refresh_obligation, refresh_reserve, RefreshObligationAccounts, RefreshReserveAccounts,
        },
        withdraw::{
            withdraw_obligation_collateral_and_redeem_reserve_collateral_v2,
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts,
        },
    },
    pda::{farms_user_state, lending_market_authority, obligation},
    KLEND_PROGRAM_ID,
};
use serde::{Deserialize, Serialize};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};
use std::{
    error::Error,
    io::{self, Read},
    str::FromStr,
};

#[derive(Clone, Default, Deserialize)]
#[serde(rename_all = "camelCase", default, deny_unknown_fields)]
struct Position {
    reserve: String,
    market: String,
    market_authority: String,
    liquidity_mint: String,
    collateral_mint: String,
    liquidity_supply: String,
    collateral_supply: String,
    liquidity_token_program: String,
    obligation: String,
    vault_liquidity_ata: String,
    pyth_oracle: String,
    switchboard_price_oracle: String,
    switchboard_twap_oracle: String,
    scope_prices: String,
    obligation_farm_user_state: String,
    reserve_farm_state: String,
    obligation_deposit_reserves: Vec<String>,
    obligation_borrow_reserves: Vec<String>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Request {
    vault: String,
    source: Position,
    target: Position,
    withdraw_collateral_amount: u64,
    deposit_liquidity_amount: u64,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Account {
    address: String,
    signer: bool,
    writable: bool,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct OutputInstruction {
    step: String,
    program: String,
    accounts: Vec<Account>,
    data_hex: String,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ProxyRequest {
    schema_version: u8,
    operation: String,
    request: Request,
}
#[derive(Serialize)]
struct RouteOutput {
    public: Vec<OutputInstruction>,
    protected: Vec<OutputInstruction>,
}
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ProxyOutput {
    schema_version: u8,
    operation: &'static str,
    route: RouteOutput,
}

fn key(value: &str) -> Result<Pubkey, Box<dyn Error>> {
    Ok(Pubkey::from_str(value)?)
}
fn bind_pdas(position: &mut Position, vault: Pubkey) -> Result<(), Box<dyn Error>> {
    let market = key(&position.market)?;
    let authority = lending_market_authority(&KLEND_PROGRAM_ID, &market)
        .0
        .to_string();
    let zero = Pubkey::default();
    let obligation_address = obligation(&KLEND_PROGRAM_ID, 0, 0, &vault, &market, &zero, &zero).0;
    if !position.market_authority.is_empty() && position.market_authority != authority {
        return Err("market authority does not match KLend PDA".into());
    }
    if !position.obligation.is_empty() && position.obligation != obligation_address.to_string() {
        return Err("obligation does not match vanilla KLend PDA".into());
    }
    position.market_authority = authority;
    position.obligation = obligation_address.to_string();
    if !position.reserve_farm_state.is_empty() {
        let user = farms_user_state(&key(&position.reserve_farm_state)?, &obligation_address)
            .0
            .to_string();
        if !position.obligation_farm_user_state.is_empty()
            && position.obligation_farm_user_state != user
        {
            return Err("farm user state does not match Farms PDA".into());
        }
        position.obligation_farm_user_state = user;
    } else if !position.obligation_farm_user_state.is_empty() {
        return Err("farm user state has no reserve farm".into());
    }
    Ok(())
}
fn optional(value: &str) -> Result<Option<Pubkey>, Box<dyn Error>> {
    if value.is_empty() {
        Ok(None)
    } else {
        Ok(Some(key(value)?))
    }
}
fn encoded(step: &str, instruction: Instruction) -> OutputInstruction {
    OutputInstruction {
        step: step.to_owned(),
        program: instruction.program_id.to_string(),
        accounts: instruction
            .accounts
            .into_iter()
            .map(|a| Account {
                address: a.pubkey.to_string(),
                signer: a.is_signer,
                writable: a.is_writable,
            })
            .collect(),
        data_hex: instruction
            .data
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect(),
    }
}
fn refresh_reserve_ix(p: &Position) -> Result<Instruction, Box<dyn Error>> {
    Ok(refresh_reserve(RefreshReserveAccounts {
        reserve: key(&p.reserve)?,
        lending_market: key(&p.market)?,
        pyth_oracle: optional(&p.pyth_oracle)?,
        switchboard_price_oracle: optional(&p.switchboard_price_oracle)?,
        switchboard_twap_oracle: optional(&p.switchboard_twap_oracle)?,
        scope_prices: optional(&p.scope_prices)?,
    }))
}
fn refresh_obligation_ix(p: &Position, target_only: bool) -> Result<Instruction, Box<dyn Error>> {
    let values = if target_only {
        vec![p.reserve.as_str()]
    } else {
        p.obligation_deposit_reserves
            .iter()
            .chain(p.obligation_borrow_reserves.iter())
            .map(String::as_str)
            .collect()
    };
    let remaining = values
        .into_iter()
        .map(|v| Ok(solana_sdk::instruction::AccountMeta::new(key(v)?, false)))
        .collect::<Result<Vec<_>, Box<dyn Error>>>()?;
    Ok(refresh_obligation(
        RefreshObligationAccounts {
            lending_market: key(&p.market)?,
            obligation: key(&p.obligation)?,
        },
        remaining,
    ))
}
fn withdraw_ix(vault: Pubkey, p: &Position, amount: u64) -> Result<Instruction, Box<dyn Error>> {
    Ok(
        withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
                owner: vault,
                obligation: key(&p.obligation)?,
                lending_market: key(&p.market)?,
                lending_market_authority: key(&p.market_authority)?,
                withdraw_reserve: key(&p.reserve)?,
                reserve_liquidity_mint: key(&p.liquidity_mint)?,
                reserve_source_collateral: key(&p.collateral_supply)?,
                reserve_collateral_mint: key(&p.collateral_mint)?,
                reserve_liquidity_supply: key(&p.liquidity_supply)?,
                user_destination_liquidity: key(&p.vault_liquidity_ata)?,
                placeholder_user_destination_collateral: None,
                liquidity_token_program: key(&p.liquidity_token_program)?,
                obligation_farm_user_state: optional(&p.obligation_farm_user_state)?,
                reserve_farm_state: optional(&p.reserve_farm_state)?,
            },
            amount,
        ),
    )
}
fn deposit_ix(vault: Pubkey, p: &Position, amount: u64) -> Result<Instruction, Box<dyn Error>> {
    Ok(deposit_reserve_liquidity_and_obligation_collateral_v2(
        DepositReserveLiquidityAndObligationCollateralV2Accounts {
            owner: vault,
            obligation: key(&p.obligation)?,
            lending_market: key(&p.market)?,
            lending_market_authority: key(&p.market_authority)?,
            reserve: key(&p.reserve)?,
            reserve_liquidity_mint: key(&p.liquidity_mint)?,
            reserve_liquidity_supply: key(&p.liquidity_supply)?,
            reserve_collateral_mint: key(&p.collateral_mint)?,
            reserve_destination_deposit_collateral: key(&p.collateral_supply)?,
            user_source_liquidity: key(&p.vault_liquidity_ata)?,
            placeholder_user_destination_collateral: None,
            liquidity_token_program: key(&p.liquidity_token_program)?,
            obligation_farm_user_state: optional(&p.obligation_farm_user_state)?,
            reserve_farm_state: optional(&p.reserve_farm_state)?,
        },
        amount,
    ))
}
fn run() -> Result<(), Box<dyn Error>> {
    let mut raw = String::new();
    io::stdin().read_to_string(&mut raw)?;
    let input: ProxyRequest = serde_json::from_str(&raw)?;
    if input.schema_version != 1 || input.operation != "buildSameMintRoute" {
        return Err("unsupported KLend proxy schema or operation".into());
    }
    let mut r = input.request;
    if r.withdraw_collateral_amount == 0
        || r.deposit_liquidity_amount == 0
        || r.source.liquidity_mint != r.target.liquidity_mint
        || r.source.vault_liquidity_ata != r.target.vault_liquidity_ata
    {
        return Err("invalid same-mint route request".into());
    }
    let vault = key(&r.vault)?;
    bind_pdas(&mut r.source, vault)?;
    bind_pdas(&mut r.target, vault)?;
    let mut public = vec![encoded(
        "kamino_refresh_reserve",
        refresh_reserve_ix(&r.source)?,
    )];
    if r.target.reserve != r.source.reserve {
        public.push(encoded(
            "kamino_refresh_reserve",
            refresh_reserve_ix(&r.target)?,
        ));
    }
    public.push(encoded(
        "kamino_refresh_obligation",
        refresh_obligation_ix(&r.source, false)?,
    ));
    let protected = vec![
        encoded(
            "kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2",
            withdraw_ix(vault, &r.source, r.withdraw_collateral_amount)?,
        ),
        encoded(
            "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2",
            deposit_ix(vault, &r.target, r.deposit_liquidity_amount)?,
        ),
    ];
    public.push(encoded(
        "kamino_refresh_obligation",
        refresh_obligation_ix(&r.target, true)?,
    ));
    println!(
        "{}",
        serde_json::to_string(&ProxyOutput {
            schema_version: 1,
            operation: "buildSameMintRoute",
            route: RouteOutput { public, protected },
        })?
    );
    Ok(())
}
fn main() {
    if let Err(e) = run() {
        eprintln!("{e}");
        std::process::exit(1)
    }
}
