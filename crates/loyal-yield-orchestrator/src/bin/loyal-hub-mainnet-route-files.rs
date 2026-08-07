use std::{env, error::Error, fs, path::PathBuf, str::FromStr};

use klend_interface::{
    from_account_data,
    instructions::{
        deposit::{
            deposit_reserve_liquidity_and_obligation_collateral_v2,
            DepositReserveLiquidityAndObligationCollateralV2Accounts,
        },
        obligation::{init_obligation, InitObligationAccounts},
        obligation::{init_obligation_farms_for_reserve, InitObligationFarmsForReserveAccounts},
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
    state::{Obligation, Reserve},
    types::InitObligationArgs,
    FARMS_PROGRAM_ID, KLEND_PROGRAM_ID,
};
use loyal_actions::{ASSOCIATED_TOKEN_PROGRAM_ID, KAMINO_MAIN_USDC_RESERVE, PYUSD_MINT, USDC_MINT};
use serde_json::{json, Value};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

const DEFAULT_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const DEFAULT_WITHDRAW_FILE: &str = "tmp/withdraw-usdc-kamino.json";
const DEFAULT_DEPOSIT_FILE: &str = "tmp/deposit-pyusd-kamino.json";
const DEFAULT_POLICY_SETUP_FILE: &str = "tmp/policy-setup-kamino-usdc.json";
const KAMINO_MAIN_PYUSD_RESERVE: &str = "2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN";
const KAMINO_COLLATERAL_FARM_MODE: u8 = 0;

#[derive(Debug)]
struct Options {
    vault: Pubkey,
    setup_fee_payer: Pubkey,
    rpc_url: String,
    source_reserve: Pubkey,
    target_reserve: Pubkey,
    setup_amount_raw: u64,
    route_withdraw_amount_raw: Option<u64>,
    route_deposit_amount_raw: u64,
    route_withdraw_file: PathBuf,
    route_deposit_file: PathBuf,
    policy_setup_file: PathBuf,
}

#[derive(Debug)]
struct ReserveSummary {
    reserve: Pubkey,
    market: Pubkey,
    liquidity_mint: Pubkey,
    liquidity_token_program: Pubkey,
    liquidity_supply: Pubkey,
    collateral_mint: Pubkey,
    collateral_supply: Pubkey,
    collateral_farm: Option<Pubkey>,
    pyth_oracle: Option<Pubkey>,
    switchboard_price_oracle: Option<Pubkey>,
    switchboard_twap_oracle: Option<Pubkey>,
    scope_prices: Option<Pubkey>,
}

#[derive(Clone, Debug)]
struct ObligationSummary {
    exists: bool,
    reserve_deposited_amount_raw: u64,
    deposit_reserves: Vec<Pubkey>,
    borrow_reserves: Vec<Pubkey>,
}

fn main() -> Result<(), Box<dyn Error>> {
    let options = match parse_args(env::args().skip(1)) {
        Ok(value) => value,
        Err(message) if message == "help" => {
            print_help();
            return Ok(());
        }
        Err(message) => return Err(message.into()),
    };

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::confirmed());
    let source = load_reserve_summary(&rpc, &options.source_reserve)?;
    let target = load_reserve_summary(&rpc, &options.target_reserve)?;
    require_mint("source", &source, &USDC_MINT)?;
    require_mint("target", &target, &PYUSD_MINT)?;

    let obligation = derive_obligation(options.vault, source.market);
    if source.market != target.market {
        return Err(format!(
            "source market {} and target market {} differ; this generator expects one vanilla Kamino obligation",
            source.market, target.market
        )
        .into());
    }
    let obligation_summary = load_obligation_summary(
        &rpc,
        &obligation,
        &options.vault,
        &source.market,
        &source.reserve,
    )?;
    let (vault_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &options.vault);
    let user_metadata_exists =
        account_exists_with_owner(&rpc, &vault_user_metadata, &KLEND_PROGRAM_ID)?;
    let source_farm_user_state = source
        .collateral_farm
        .map(|farm| farms_user_state(&farm, &obligation).0);
    let source_farm_user_state_exists = source_farm_user_state
        .map(|account| account_exists_with_owner(&rpc, &account, &FARMS_PROGRAM_ID))
        .transpose()?
        .unwrap_or(true);
    let target_farm_user_state = target
        .collateral_farm
        .map(|farm| farms_user_state(&farm, &obligation).0);
    let target_farm_user_state_exists = target_farm_user_state
        .map(|account| account_exists_with_owner(&rpc, &account, &FARMS_PROGRAM_ID))
        .transpose()?
        .unwrap_or(true);
    let withdraw_amount = options
        .route_withdraw_amount_raw
        .or_else(|| {
            (obligation_summary.reserve_deposited_amount_raw > 0)
                .then_some(obligation_summary.reserve_deposited_amount_raw)
        })
        .unwrap_or(options.setup_amount_raw);

    let setup_instructions = build_policy_setup_instructions(
        options.vault,
        &source,
        &target,
        &obligation_summary,
        user_metadata_exists,
        source_farm_user_state_exists,
        options.setup_fee_payer,
        options.setup_amount_raw,
    )?;
    let route_withdraw_instructions = build_route_withdraw_instructions(
        options.vault,
        &source,
        &target,
        &obligation_summary,
        withdraw_amount,
    )?;
    let route_deposit_obligation_summary = route_deposit_obligation_summary_after_withdraw(
        &obligation_summary,
        &source.reserve,
        withdraw_amount,
    );
    let route_deposit_instructions = build_route_deposit_instructions(
        options.vault,
        options.setup_fee_payer,
        &source,
        &target,
        &route_deposit_obligation_summary,
        target_farm_user_state_exists,
        options.route_deposit_amount_raw,
    )?;

    write_wire_instructions(&options.policy_setup_file, &setup_instructions)?;
    write_wire_instructions(&options.route_withdraw_file, &route_withdraw_instructions)?;
    write_wire_instructions(&options.route_deposit_file, &route_deposit_instructions)?;

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "generated",
            "vault": options.vault.to_string(),
            "setupFeePayer": options.setup_fee_payer.to_string(),
            "obligation": obligation.to_string(),
            "userMetadata": vault_user_metadata.to_string(),
            "userMetadataExisted": user_metadata_exists,
            "sourceFarmUserState": source_farm_user_state.map(|account| account.to_string()),
            "sourceFarmUserStateExisted": source_farm_user_state_exists,
            "targetFarmUserState": target_farm_user_state.map(|account| account.to_string()),
            "targetFarmUserStateExisted": target_farm_user_state_exists,
            "sourceReserve": source.reserve.to_string(),
            "targetReserve": target.reserve.to_string(),
            "setupAmountRaw": options.setup_amount_raw.to_string(),
            "withdrawAmountRaw": withdraw_amount.to_string(),
            "depositAmountRaw": options.route_deposit_amount_raw.to_string(),
            "obligationExisted": obligation_summary.exists,
            "routeDepositAssumesObligationClosed": !route_deposit_obligation_summary.exists && obligation_summary.exists,
            "files": {
                "policySetup": options.policy_setup_file,
                "routeWithdraw": options.route_withdraw_file,
                "routeDeposit": options.route_deposit_file,
            },
        }))?
    );

    Ok(())
}

fn route_deposit_obligation_summary_after_withdraw(
    summary: &ObligationSummary,
    source_reserve: &Pubkey,
    withdraw_amount: u64,
) -> ObligationSummary {
    if withdraw_closes_only_obligation_position(summary, source_reserve, withdraw_amount) {
        return ObligationSummary {
            exists: false,
            reserve_deposited_amount_raw: 0,
            deposit_reserves: Vec::new(),
            borrow_reserves: Vec::new(),
        };
    }
    summary.clone()
}

fn withdraw_closes_only_obligation_position(
    summary: &ObligationSummary,
    source_reserve: &Pubkey,
    withdraw_amount: u64,
) -> bool {
    summary.exists
        && summary.reserve_deposited_amount_raw > 0
        && withdraw_amount >= summary.reserve_deposited_amount_raw
        && summary.borrow_reserves.is_empty()
        && !summary.deposit_reserves.is_empty()
        && summary
            .deposit_reserves
            .iter()
            .all(|reserve| reserve == source_reserve)
}

fn build_route_withdraw_instructions(
    vault: Pubkey,
    source: &ReserveSummary,
    target: &ReserveSummary,
    obligation_summary: &ObligationSummary,
    amount: u64,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    let mut instructions = build_refresh_route_reserve_instructions(source, target);
    instructions.push(build_refresh_obligation_instruction(
        source.market,
        &derive_obligation(vault, source.market),
        obligation_summary,
    ));
    instructions.push(build_withdraw_instruction(vault, source, amount)?);
    Ok(instructions)
}

fn build_route_deposit_instructions(
    vault: Pubkey,
    setup_fee_payer: Pubkey,
    source: &ReserveSummary,
    target: &ReserveSummary,
    obligation_summary: &ObligationSummary,
    target_farm_user_state_exists: bool,
    amount: u64,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    let mut instructions = Vec::new();
    if !obligation_summary.exists {
        instructions.push(build_init_obligation_instruction(
            vault,
            setup_fee_payer,
            target.market,
        ));
    }
    if !target_farm_user_state_exists {
        if let Some(collateral_farm) = target.collateral_farm {
            instructions.push(build_init_obligation_farm_instruction(
                vault,
                setup_fee_payer,
                target,
                collateral_farm,
            ));
        }
    }
    instructions.extend(build_refresh_route_reserve_instructions(source, target));
    instructions.push(build_refresh_obligation_instruction(
        target.market,
        &derive_obligation(vault, target.market),
        obligation_summary,
    ));
    instructions.push(build_deposit_instruction(vault, target, amount)?);
    Ok(instructions)
}

#[allow(clippy::too_many_arguments)]
fn build_policy_setup_instructions(
    vault: Pubkey,
    source: &ReserveSummary,
    target: &ReserveSummary,
    obligation_summary: &ObligationSummary,
    user_metadata_exists: bool,
    source_farm_user_state_exists: bool,
    setup_fee_payer: Pubkey,
    amount: u64,
) -> Result<Vec<Instruction>, Box<dyn Error>> {
    let mut instructions = Vec::new();
    if !user_metadata_exists {
        instructions.push(build_init_user_metadata_instruction(vault, setup_fee_payer));
    }
    if !obligation_summary.exists {
        instructions.push(build_init_obligation_instruction(
            vault,
            setup_fee_payer,
            source.market,
        ));
    }
    if !source_farm_user_state_exists {
        if let Some(collateral_farm) = source.collateral_farm {
            instructions.push(build_init_obligation_farm_instruction(
                vault,
                setup_fee_payer,
                source,
                collateral_farm,
            ));
        }
    }
    instructions.extend(build_refresh_route_reserve_instructions(source, target));
    instructions.push(build_refresh_obligation_instruction(
        source.market,
        &derive_obligation(vault, source.market),
        obligation_summary,
    ));
    instructions.push(build_deposit_instruction(vault, source, amount)?);
    Ok(instructions)
}

fn build_refresh_route_reserve_instructions(
    source: &ReserveSummary,
    target: &ReserveSummary,
) -> Vec<Instruction> {
    let mut instructions = vec![build_refresh_reserve_instruction(source)];
    if target.reserve != source.reserve {
        instructions.push(build_refresh_reserve_instruction(target));
    }
    instructions
}

fn build_init_user_metadata_instruction(vault: Pubkey, fee_payer: Pubkey) -> Instruction {
    let (user_metadata_account, _) = user_metadata(&KLEND_PROGRAM_ID, &vault);
    init_user_metadata(
        InitUserMetadataAccounts {
            owner: vault,
            fee_payer,
            user_metadata: user_metadata_account,
            referrer_user_metadata: None,
        },
        Pubkey::default(),
    )
}

fn build_init_obligation_instruction(
    vault: Pubkey,
    fee_payer: Pubkey,
    market: Pubkey,
) -> Instruction {
    let (owner_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault);
    init_obligation(
        InitObligationAccounts {
            obligation_owner: vault,
            fee_payer,
            obligation: derive_obligation(vault, market),
            lending_market: market,
            seed1_account: Pubkey::default(),
            seed2_account: Pubkey::default(),
            owner_user_metadata,
        },
        InitObligationArgs { tag: 0, id: 0 },
    )
}

fn build_init_obligation_farm_instruction(
    vault: Pubkey,
    payer: Pubkey,
    source: &ReserveSummary,
    reserve_farm_state: Pubkey,
) -> Instruction {
    let obligation = derive_obligation(vault, source.market);
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &source.market);
    let (obligation_farm, _) = farms_user_state(&reserve_farm_state, &obligation);
    init_obligation_farms_for_reserve(
        InitObligationFarmsForReserveAccounts {
            payer,
            owner: vault,
            obligation,
            lending_market_authority,
            reserve: source.reserve,
            reserve_farm_state,
            obligation_farm,
            lending_market: source.market,
        },
        KAMINO_COLLATERAL_FARM_MODE,
    )
}

fn build_withdraw_instruction(
    vault: Pubkey,
    source: &ReserveSummary,
    amount: u64,
) -> Result<Instruction, Box<dyn Error>> {
    let obligation = derive_obligation(vault, source.market);
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &source.market);
    let (obligation_farm_user_state, reserve_farm_state) =
        collateral_farm_accounts(source.collateral_farm, &obligation);
    Ok(
        withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
                owner: vault,
                obligation,
                lending_market: source.market,
                lending_market_authority,
                withdraw_reserve: source.reserve,
                reserve_liquidity_mint: source.liquidity_mint,
                reserve_source_collateral: source.collateral_supply,
                reserve_collateral_mint: source.collateral_mint,
                reserve_liquidity_supply: source.liquidity_supply,
                user_destination_liquidity: derive_associated_token_address(
                    &vault,
                    &source.liquidity_mint,
                    &source.liquidity_token_program,
                ),
                placeholder_user_destination_collateral: None,
                liquidity_token_program: source.liquidity_token_program,
                obligation_farm_user_state,
                reserve_farm_state,
            },
            amount,
        ),
    )
}

fn build_deposit_instruction(
    vault: Pubkey,
    target: &ReserveSummary,
    amount: u64,
) -> Result<Instruction, Box<dyn Error>> {
    let obligation = derive_obligation(vault, target.market);
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &target.market);
    let (obligation_farm_user_state, reserve_farm_state) =
        collateral_farm_accounts(target.collateral_farm, &obligation);
    Ok(deposit_reserve_liquidity_and_obligation_collateral_v2(
        DepositReserveLiquidityAndObligationCollateralV2Accounts {
            owner: vault,
            obligation,
            lending_market: target.market,
            lending_market_authority,
            reserve: target.reserve,
            reserve_liquidity_mint: target.liquidity_mint,
            reserve_liquidity_supply: target.liquidity_supply,
            reserve_collateral_mint: target.collateral_mint,
            reserve_destination_deposit_collateral: target.collateral_supply,
            user_source_liquidity: derive_associated_token_address(
                &vault,
                &target.liquidity_mint,
                &target.liquidity_token_program,
            ),
            placeholder_user_destination_collateral: None,
            liquidity_token_program: target.liquidity_token_program,
            obligation_farm_user_state,
            reserve_farm_state,
        },
        amount,
    ))
}

fn build_refresh_reserve_instruction(position: &ReserveSummary) -> Instruction {
    refresh_reserve(RefreshReserveAccounts {
        reserve: position.reserve,
        lending_market: position.market,
        pyth_oracle: position.pyth_oracle,
        switchboard_price_oracle: position.switchboard_price_oracle,
        switchboard_twap_oracle: position.switchboard_twap_oracle,
        scope_prices: position.scope_prices,
    })
}

fn build_refresh_obligation_instruction(
    market: Pubkey,
    obligation: &Pubkey,
    summary: &ObligationSummary,
) -> Instruction {
    let remaining_accounts = summary
        .deposit_reserves
        .iter()
        .chain(summary.borrow_reserves.iter())
        .map(|reserve| AccountMeta::new(*reserve, false))
        .collect();
    refresh_obligation(
        RefreshObligationAccounts {
            lending_market: market,
            obligation: *obligation,
        },
        remaining_accounts,
    )
}

fn load_reserve_summary(
    rpc: &RpcClient,
    reserve: &Pubkey,
) -> Result<ReserveSummary, Box<dyn Error>> {
    let account = rpc.get_account(reserve)?;
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "reserve {reserve} is owned by {}, expected {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let reserve_state = from_account_data::<Reserve>(&account.data)?;
    Ok(ReserveSummary {
        reserve: *reserve,
        market: reserve_state.lending_market,
        liquidity_mint: reserve_state.liquidity.mint_pubkey,
        liquidity_token_program: reserve_state.liquidity.token_program,
        liquidity_supply: reserve_state.liquidity.supply_vault,
        collateral_mint: reserve_state.collateral.mint_pubkey,
        collateral_supply: reserve_state.collateral.supply_vault,
        collateral_farm: non_default_pubkey(reserve_state.farm_collateral),
        pyth_oracle: non_default_pubkey(reserve_state.config.token_info.pyth_configuration.price),
        switchboard_price_oracle: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .switchboard_configuration
                .price_aggregator,
        ),
        switchboard_twap_oracle: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .switchboard_configuration
                .twap_aggregator,
        ),
        scope_prices: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .scope_configuration
                .price_feed,
        ),
    })
}

fn account_exists_with_owner(
    rpc: &RpcClient,
    account: &Pubkey,
    owner: &Pubkey,
) -> Result<bool, Box<dyn Error>> {
    let response = rpc.get_account_with_commitment(account, CommitmentConfig::confirmed())?;
    Ok(response
        .value
        .map(|account| account.owner == *owner)
        .unwrap_or(false))
}

fn load_obligation_summary(
    rpc: &RpcClient,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
) -> Result<ObligationSummary, Box<dyn Error>> {
    let response =
        rpc.get_account_with_commitment(obligation_account, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(ObligationSummary {
            exists: false,
            reserve_deposited_amount_raw: 0,
            deposit_reserves: Vec::new(),
            borrow_reserves: Vec::new(),
        });
    };
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "obligation {obligation_account} is owned by {}, expected {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let obligation_state = from_account_data::<Obligation>(&account.data)?;
    if obligation_state.owner != *expected_owner {
        return Err(format!(
            "obligation {obligation_account} owner {} does not match vault {}",
            obligation_state.owner, expected_owner
        )
        .into());
    }
    if obligation_state.lending_market != *expected_market {
        return Err(format!(
            "obligation {obligation_account} market {} does not match reserve market {}",
            obligation_state.lending_market, expected_market
        )
        .into());
    }
    let reserve_deposited_amount_raw = obligation_state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == *reserve)
        .map(|deposit| deposit.deposited_amount)
        .unwrap_or_default();
    let deposit_reserves = obligation_state
        .deposits
        .iter()
        .filter(|deposit| deposit.deposit_reserve != Pubkey::default())
        .map(|deposit| deposit.deposit_reserve)
        .collect();
    let borrow_reserves = obligation_state
        .borrows
        .iter()
        .filter(|borrow| borrow.borrow_reserve != Pubkey::default())
        .map(|borrow| borrow.borrow_reserve)
        .collect();
    Ok(ObligationSummary {
        exists: true,
        reserve_deposited_amount_raw,
        deposit_reserves,
        borrow_reserves,
    })
}

fn derive_obligation(vault: Pubkey, market: Pubkey) -> Pubkey {
    obligation(
        &KLEND_PROGRAM_ID,
        0,
        0,
        &vault,
        &market,
        &Pubkey::default(),
        &Pubkey::default(),
    )
    .0
}

fn collateral_farm_accounts(
    collateral_farm: Option<Pubkey>,
    obligation: &Pubkey,
) -> (Option<Pubkey>, Option<Pubkey>) {
    let Some(reserve_farm_state) = collateral_farm else {
        return (None, None);
    };
    let (obligation_farm_user_state, _) = farms_user_state(&reserve_farm_state, obligation);
    (Some(obligation_farm_user_state), Some(reserve_farm_state))
}

fn require_mint(
    label: &str,
    reserve: &ReserveSummary,
    mint: &Pubkey,
) -> Result<(), Box<dyn Error>> {
    if reserve.liquidity_mint != *mint {
        return Err(format!(
            "{label} reserve {} liquidity mint {} does not match expected {}",
            reserve.reserve, reserve.liquidity_mint, mint
        )
        .into());
    }
    Ok(())
}

fn non_default_pubkey(pubkey: Pubkey) -> Option<Pubkey> {
    (pubkey != Pubkey::default()).then_some(pubkey)
}

fn derive_associated_token_address(
    owner: &Pubkey,
    mint: &Pubkey,
    token_program: &Pubkey,
) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn write_wire_instructions(
    path: &PathBuf,
    instructions: &[Instruction],
) -> Result<(), Box<dyn Error>> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let value = Value::Array(instructions.iter().map(wire_instruction_json).collect());
    fs::write(path, format!("{}\n", serde_json::to_string_pretty(&value)?))?;
    Ok(())
}

fn wire_instruction_json(instruction: &Instruction) -> Value {
    json!({
        "programId": instruction.program_id.to_string(),
        "accounts": instruction.accounts.iter().map(wire_account_json).collect::<Vec<_>>(),
        "data": instruction.data,
        "encoding": "bytes",
    })
}

fn wire_account_json(account: &AccountMeta) -> Value {
    json!({
        "pubkey": account.pubkey.to_string(),
        "isSigner": account.is_signer,
        "isWritable": account.is_writable,
    })
}

fn parse_args(values: impl IntoIterator<Item = String>) -> Result<Options, String> {
    let mut vault = None;
    let mut setup_fee_payer = None;
    let mut rpc_url = DEFAULT_RPC_URL.to_owned();
    let mut source_reserve = KAMINO_MAIN_USDC_RESERVE;
    let mut target_reserve =
        Pubkey::from_str(KAMINO_MAIN_PYUSD_RESERVE).expect("default PYUSD reserve");
    let mut setup_amount_raw = 1_000_000;
    let mut route_withdraw_amount_raw = None;
    let mut route_deposit_amount_raw = 995_000;
    let mut route_withdraw_file = PathBuf::from(DEFAULT_WITHDRAW_FILE);
    let mut route_deposit_file = PathBuf::from(DEFAULT_DEPOSIT_FILE);
    let mut policy_setup_file = PathBuf::from(DEFAULT_POLICY_SETUP_FILE);

    let mut iter = values.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--vault" => {
                vault = Some(parse_pubkey(
                    &iter.next().ok_or("--vault requires a value")?,
                    "vault",
                )?);
            }
            "--setup-fee-payer" => {
                setup_fee_payer = Some(parse_pubkey(
                    &iter.next().ok_or("--setup-fee-payer requires a value")?,
                    "setup-fee-payer",
                )?);
            }
            "--rpc-url" => rpc_url = iter.next().ok_or("--rpc-url requires a value")?,
            "--source-reserve" => {
                source_reserve = parse_pubkey(
                    &iter.next().ok_or("--source-reserve requires a value")?,
                    "source-reserve",
                )?;
            }
            "--target-reserve" => {
                target_reserve = parse_pubkey(
                    &iter.next().ok_or("--target-reserve requires a value")?,
                    "target-reserve",
                )?;
            }
            "--setup-amount-raw" => {
                setup_amount_raw = parse_u64(
                    &iter.next().ok_or("--setup-amount-raw requires a value")?,
                    "setup-amount-raw",
                )?;
            }
            "--route-withdraw-amount-raw" => {
                route_withdraw_amount_raw = Some(parse_u64(
                    &iter
                        .next()
                        .ok_or("--route-withdraw-amount-raw requires a value")?,
                    "route-withdraw-amount-raw",
                )?);
            }
            "--route-deposit-amount-raw" => {
                route_deposit_amount_raw = parse_u64(
                    &iter
                        .next()
                        .ok_or("--route-deposit-amount-raw requires a value")?,
                    "route-deposit-amount-raw",
                )?;
            }
            "--route-withdraw-file" => {
                route_withdraw_file = PathBuf::from(
                    iter.next()
                        .ok_or("--route-withdraw-file requires a value")?,
                );
            }
            "--route-deposit-file" => {
                route_deposit_file =
                    PathBuf::from(iter.next().ok_or("--route-deposit-file requires a value")?);
            }
            "--policy-setup-file" => {
                policy_setup_file =
                    PathBuf::from(iter.next().ok_or("--policy-setup-file requires a value")?);
            }
            "--help" | "-h" => return Err("help".to_owned()),
            _ => return Err(format!("unknown argument: {arg}")),
        }
    }

    let vault = vault.ok_or("--vault is required")?;
    Ok(Options {
        vault,
        setup_fee_payer: setup_fee_payer.unwrap_or(vault),
        rpc_url,
        source_reserve,
        target_reserve,
        setup_amount_raw,
        route_withdraw_amount_raw,
        route_deposit_amount_raw,
        route_withdraw_file,
        route_deposit_file,
        policy_setup_file,
    })
}

fn parse_pubkey(value: &str, label: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(value).map_err(|_| format!("--{label} must be a public key"))
}

fn parse_u64(value: &str, label: &str) -> Result<u64, String> {
    value
        .parse()
        .map_err(|_| format!("--{label} must be an unsigned integer"))
}

fn print_help() {
    println!(
        "Usage: loyal-hub-mainnet-route-files --vault <PUBKEY> [--setup-fee-payer <PUBKEY>] [--rpc-url <URL>] [--setup-amount-raw <N>] [--route-deposit-amount-raw <N>]\n\n\
         Generates tmp/withdraw-usdc-kamino.json, tmp/deposit-pyusd-kamino.json, and tmp/policy-setup-kamino-usdc.json for the Loyal Hub mainnet runner."
    );
}

#[cfg(test)]
mod tests {
    use super::*;

    fn obligation_summary(
        amount: u64,
        deposit_reserves: Vec<Pubkey>,
        borrow_reserves: Vec<Pubkey>,
    ) -> ObligationSummary {
        ObligationSummary {
            exists: true,
            reserve_deposited_amount_raw: amount,
            deposit_reserves,
            borrow_reserves,
        }
    }

    #[test]
    fn route_deposit_reinitializes_obligation_after_full_only_position_withdraw() {
        let source_reserve = Pubkey::new_unique();
        let summary = obligation_summary(841_542, vec![source_reserve], Vec::new());

        let deposit_summary =
            route_deposit_obligation_summary_after_withdraw(&summary, &source_reserve, 841_542);

        assert!(!deposit_summary.exists);
        assert!(deposit_summary.deposit_reserves.is_empty());
        assert!(deposit_summary.borrow_reserves.is_empty());
    }

    #[test]
    fn route_deposit_keeps_existing_obligation_after_partial_withdraw() {
        let source_reserve = Pubkey::new_unique();
        let summary = obligation_summary(841_542, vec![source_reserve], Vec::new());

        let deposit_summary =
            route_deposit_obligation_summary_after_withdraw(&summary, &source_reserve, 841_541);

        assert!(deposit_summary.exists);
        assert_eq!(deposit_summary.deposit_reserves, vec![source_reserve]);
    }

    #[test]
    fn route_deposit_keeps_existing_obligation_with_other_positions() {
        let source_reserve = Pubkey::new_unique();
        let other_reserve = Pubkey::new_unique();
        let summary = obligation_summary(841_542, vec![source_reserve, other_reserve], Vec::new());

        let deposit_summary =
            route_deposit_obligation_summary_after_withdraw(&summary, &source_reserve, 841_542);

        assert!(deposit_summary.exists);
        assert_eq!(
            deposit_summary.deposit_reserves,
            vec![source_reserve, other_reserve]
        );
    }
}
