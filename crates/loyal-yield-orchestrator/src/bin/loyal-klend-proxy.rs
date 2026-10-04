use klend_interface::{
    instructions::{
        deposit::{
            deposit_reserve_liquidity_and_obligation_collateral_v2,
            DepositReserveLiquidityAndObligationCollateralV2Accounts,
        },
        obligation::{
            init_obligation, init_obligation_farms_for_reserve, InitObligationAccounts,
            InitObligationFarmsForReserveAccounts,
        },
        referrer::{init_user_metadata, InitUserMetadataAccounts},
        refresh::{
            refresh_obligation, refresh_reserve, RefreshObligationAccounts, RefreshReserveAccounts,
        },
        withdraw::{
            withdraw_obligation_collateral_and_redeem_reserve_collateral_v2,
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts,
        },
    },
    pda::{farms_user_state, lending_market_authority, obligation, user_metadata},
    types::InitObligationArgs,
    KLEND_PROGRAM_ID,
};
use serde::{Deserialize, Serialize};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
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
#[serde(tag = "operation", deny_unknown_fields)]
enum ProxyRequest {
    #[serde(rename = "buildCanonicalSubscriptionPolicy")]
    CanonicalSubscriptionPolicy {
        #[serde(rename = "schemaVersion")]
        schema_version: u8,
        request: CanonicalSubscriptionPolicyRequest,
    },
    #[serde(rename = "buildDestinationSetup")]
    DestinationSetup {
        #[serde(rename = "schemaVersion")]
        schema_version: u8,
        request: DestinationSetupRequest,
    },
    #[serde(rename = "buildSameMintRoute")]
    SameMint {
        #[serde(rename = "schemaVersion")]
        schema_version: u8,
        request: Request,
    },
    #[serde(rename = "buildCrossMintLegs")]
    CrossMint {
        #[serde(rename = "schemaVersion")]
        schema_version: u8,
        request: Request,
    },
    #[serde(rename = "buildIdleDeposit")]
    IdleDeposit {
        #[serde(rename = "schemaVersion")]
        schema_version: u8,
        request: IdleDepositRequest,
    },
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct DestinationSetupRequest {
    stage: String,
    vault: String,
    payer: String,
    target: Position,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct CanonicalSubscriptionPolicyRequest {
    settings: String,
    root_authority: String,
    payer: String,
    delegated_signer: String,
    policy_seed: u64,
    wallet: String,
    vault: String,
    max_amount_per_period: u64,
    policy_data_hex: String,
}

fn build_canonical_subscription_policy_json(
    r: CanonicalSubscriptionPolicyRequest,
) -> Result<String, Box<dyn Error>> {
    let settings = key(&r.settings)?;
    let signer = key(&r.delegated_signer)?;
    let ix = loyal_actions::build_canonical_subscription_sweep_policy_create(
        settings,
        key(&r.root_authority)?,
        key(&r.payer)?,
        signer,
        r.policy_seed,
        key(&r.wallet)?,
        key(&r.vault)?,
        r.max_amount_per_period,
    )?;
    if !r.policy_data_hex.is_empty() {
        if r.policy_data_hex.len() > 65536 || r.policy_data_hex.len() % 2 != 0 {
            return Err("policy account hex exceeds bound or has odd length".into());
        }
        let raw = r
            .policy_data_hex
            .as_bytes()
            .chunks_exact(2)
            .map(|chunk| -> Result<u8, Box<dyn Error>> {
                Ok(u8::from_str_radix(std::str::from_utf8(chunk)?, 16)?)
            })
            .collect::<Result<Vec<_>, _>>()?;
        let Some(mut current) = loyal_actions::decode_program_interaction_policy_account(&raw)?
        else {
            return Err(
                "current subscription policy lacks complete canonical security properties".into(),
            );
        };
        let expected = loyal_actions::decode_squads_policy_create_actions(&ix)?;
        if expected.len() != 1
            || current.settings != settings
            || current.policy_seed != r.policy_seed
            || current.policy_account != ix.accounts[5].pubkey
            || current.delegated_signer != signer
            || current.threshold != 1
        {
            return Err("current subscription policy header differs from canonical target".into());
        }
        // The strict account decoder already validates tight compact tables;
        // account/data predicates have resolved pubkeys, so compare semantics.
        current.payload.pubkey_table.clear();
        if current.payload != expected[0].payload {
            return Err(
                "current subscription policy full constraint matrix differs from canonical target"
                    .into(),
            );
        }
    }
    Ok(serde_json::to_string(&ProxyOutput {
        schema_version: 1,
        operation: "buildCanonicalSubscriptionPolicy",
        route: RouteOutput {
            public: vec![],
            protected: vec![encoded("canonical_subscription_policy_create", ix)],
        },
    })?)
}

fn build_destination_setup_json(mut r: DestinationSetupRequest) -> Result<String, Box<dyn Error>> {
    let vault = key(&r.vault)?;
    let payer = key(&r.payer)?;
    bind_pdas(&mut r.target, vault)?;
    let metadata = user_metadata(&KLEND_PROGRAM_ID, &vault).0;
    let ix = match r.stage.as_str() {
        // This is the same source-owned idempotent ATA instruction as
        // autonomous-vaults/src/kamino.rs, with the executor paying rent.
        "ata" => {
            let ata = loyal_actions::derive_associated_token_account(
                vault,
                key(&r.target.liquidity_mint)?,
                key(&r.target.liquidity_token_program)?,
            );
            if ata.to_string() != r.target.vault_liquidity_ata {
                return Err("setup custody is not vault ATA".into());
            }
            Instruction {
                program_id: key("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")?,
                accounts: vec![
                    AccountMeta::new(payer, true),
                    AccountMeta::new(ata, false),
                    AccountMeta::new_readonly(vault, false),
                    AccountMeta::new_readonly(key(&r.target.liquidity_mint)?, false),
                    AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
                    AccountMeta::new_readonly(key(&r.target.liquidity_token_program)?, false),
                ],
                data: vec![1],
            }
        }
        "metadata" => init_user_metadata(
            InitUserMetadataAccounts {
                owner: vault,
                fee_payer: vault,
                user_metadata: metadata,
                referrer_user_metadata: None,
            },
            Pubkey::default(),
        ),
        "obligation" => init_obligation(
            InitObligationAccounts {
                obligation_owner: vault,
                fee_payer: vault,
                obligation: key(&r.target.obligation)?,
                lending_market: key(&r.target.market)?,
                seed1_account: Pubkey::default(),
                seed2_account: Pubkey::default(),
                owner_user_metadata: metadata,
            },
            InitObligationArgs { tag: 0, id: 0 },
        ),
        "farm" => init_obligation_farms_for_reserve(
            InitObligationFarmsForReserveAccounts {
                payer,
                owner: vault,
                obligation: key(&r.target.obligation)?,
                lending_market_authority: key(&r.target.market_authority)?,
                reserve: key(&r.target.reserve)?,
                reserve_farm_state: key(&r.target.reserve_farm_state)?,
                obligation_farm: key(&r.target.obligation_farm_user_state)?,
                lending_market: key(&r.target.market)?,
            },
            0,
        ),
        _ => return Err("unsupported destination setup stage".into()),
    };
    let ix = encoded(&format!("kamino_setup_{}", r.stage), ix);
    let (public, protected) = if matches!(r.stage.as_str(), "metadata" | "obligation") {
        (vec![], vec![ix])
    } else {
        (vec![ix], vec![])
    };
    Ok(serde_json::to_string(&ProxyOutput {
        schema_version: 1,
        operation: "buildDestinationSetup",
        route: RouteOutput { public, protected },
    })?)
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct IdleDepositRequest {
    vault: String,
    target: Position,
    deposit_liquidity_amount: u64,
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
fn build_idle_json(mut r: IdleDepositRequest) -> Result<String, Box<dyn Error>> {
    if r.deposit_liquidity_amount == 0
        || r.target.obligation.is_empty()
        || r.target.market_authority.is_empty()
        || !r.target.obligation_borrow_reserves.is_empty()
        || r.target.obligation_deposit_reserves.len() > 1
        || r.target
            .obligation_deposit_reserves
            .iter()
            .any(|reserve| reserve != &r.target.reserve)
    {
        return Err("invalid idle deposit amount or obligation footprint".into());
    }
    let vault = key(&r.vault)?;
    bind_pdas(&mut r.target, vault)?;
    let public = vec![
        encoded("kamino_refresh_reserve", refresh_reserve_ix(&r.target)?),
        encoded(
            "kamino_refresh_obligation",
            refresh_obligation_ix(&r.target, false)?,
        ),
    ];
    let protected = vec![encoded(
        "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2",
        deposit_ix(vault, &r.target, r.deposit_liquidity_amount)?,
    )];
    Ok(serde_json::to_string(&ProxyOutput {
        schema_version: 1,
        operation: "buildIdleDeposit",
        route: RouteOutput { public, protected },
    })?)
}

pub fn build_json(raw: &str) -> Result<String, Box<dyn Error>> {
    let input: ProxyRequest = serde_json::from_str(raw)?;
    let (operation, mut r) = match input {
        ProxyRequest::CanonicalSubscriptionPolicy {
            schema_version: 1,
            request,
        } => return build_canonical_subscription_policy_json(request),
        ProxyRequest::DestinationSetup {
            schema_version: 1,
            request,
        } => return build_destination_setup_json(request),
        ProxyRequest::SameMint {
            schema_version: 1,
            request,
        } => ("buildSameMintRoute", request),
        ProxyRequest::CrossMint {
            schema_version: 1,
            request,
        } => ("buildCrossMintLegs", request),
        ProxyRequest::IdleDeposit {
            schema_version: 1,
            request,
        } => return build_idle_json(request),
        _ => return Err("unsupported KLend proxy schema".into()),
    };
    let same_mint = r.source.liquidity_mint == r.target.liquidity_mint;
    let same_ata = r.source.vault_liquidity_ata == r.target.vault_liquidity_ata;
    let valid_lane = match operation {
        "buildSameMintRoute" => same_mint && same_ata,
        "buildCrossMintLegs" => !same_mint && !same_ata,
        _ => false,
    };
    if r.withdraw_collateral_amount == 0 || r.deposit_liquidity_amount == 0 || !valid_lane {
        return Err("invalid KLend route lane or amount".into());
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
    Ok(serde_json::to_string(&ProxyOutput {
        schema_version: 1,
        operation,
        route: RouteOutput { public, protected },
    })?)
}
fn run() -> Result<(), Box<dyn Error>> {
    let mut raw = String::new();
    io::stdin().read_to_string(&mut raw)?;
    println!("{}", build_json(&raw)?);
    Ok(())
}
fn main() {
    if let Err(e) = run() {
        eprintln!("{e}");
        std::process::exit(1)
    }
}
