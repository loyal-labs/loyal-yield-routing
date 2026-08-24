use std::{
    env, fs,
    path::{Path, PathBuf},
};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use bytemuck::{bytes_of, Zeroable};
use klend_interface::state::{Obligation, Reserve, SplDiscriminate};
use solana_sdk::{
    hash::hashv,
    program_option::COption,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
};
use spl_associated_token_account::get_associated_token_address_with_program_id;
use spl_token::{
    solana_program::program_pack::Pack,
    state::{Account as TokenAccount, AccountState, Mint},
};
use squads_test_harness::{
    derive_squads_program_config, derive_squads_settings, derive_squads_vault,
    serialize_squads_program_config, squads_test_treasury, KAMINO_LENDING_MARKET_AUTHORITY_SEED,
    KAMINO_LEND_PROGRAM_ID, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_DECIMALS, USDC_MINT,
};

const GENESIS_LAMPORTS: u64 = 1_000_000_000;
const RESERVE_LIQUIDITY_RAW: u64 = 1_000_000_000_000;
const MINT_AUTHORITY_SEED: [u8; 32] = [7; 32];

fn deterministic_pubkey(label: &[u8]) -> Pubkey {
    Pubkey::new_from_array(hashv(&[label]).to_bytes())
}

fn derive_obligation(vault: &Pubkey, market: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[
            &[0],
            &[0],
            vault.as_ref(),
            market.as_ref(),
            Pubkey::default().as_ref(),
            Pubkey::default().as_ref(),
        ],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

fn write_account(
    output_dir: &Path,
    name: &str,
    pubkey: Pubkey,
    owner: Pubkey,
    data: Vec<u8>,
) -> Result<PathBuf, Box<dyn std::error::Error>> {
    let output = output_dir.join(format!("{name}.json"));
    let account = serde_json::json!({
        "account": {
            "data": [BASE64_STANDARD.encode(&data), "base64"],
            "executable": false,
            "lamports": GENESIS_LAMPORTS,
            "owner": owner.to_string(),
            "rentEpoch": 0,
            "space": data.len(),
        },
        "pubkey": pubkey.to_string(),
    });
    fs::write(&output, serde_json::to_vec_pretty(&account)?)?;
    Ok(output)
}

fn mint_data(
    authority: Option<Pubkey>,
    supply: u64,
    decimals: u8,
) -> Result<Vec<u8>, Box<dyn std::error::Error>> {
    let mut data = vec![0; Mint::LEN];
    Mint::pack(
        Mint {
            mint_authority: authority.map_or(COption::None, COption::Some),
            supply,
            decimals,
            is_initialized: true,
            freeze_authority: COption::None,
        },
        &mut data,
    )?;
    Ok(data)
}

fn token_account_data(
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
) -> Result<Vec<u8>, Box<dyn std::error::Error>> {
    let mut data = vec![0; TokenAccount::LEN];
    TokenAccount::pack(
        TokenAccount {
            mint,
            owner,
            amount,
            delegate: COption::None,
            state: AccountState::Initialized,
            is_native: COption::None,
            delegated_amount: 0,
            close_authority: COption::None,
        },
        &mut data,
    )?;
    Ok(data)
}

fn klend_data<T: bytemuck::Pod + SplDiscriminate>(value: &T) -> Vec<u8> {
    let mut data = Vec::with_capacity(8 + std::mem::size_of::<T>());
    data.extend_from_slice(T::SPL_DISCRIMINATOR_SLICE);
    data.extend_from_slice(bytes_of(value));
    data
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let output_dir = env::args_os()
        .nth(1)
        .map(PathBuf::from)
        .ok_or("output directory is required")?;
    let manifest_output = env::args_os()
        .nth(2)
        .map(PathBuf::from)
        .ok_or("manifest output path is required")?;
    fs::create_dir_all(&output_dir)?;

    let settings = derive_squads_settings(1).0;
    let vault = derive_squads_vault(&settings, 1).0;
    let obligation = derive_obligation(&vault, &KAMINO_MAIN_MARKET);
    let market_authority = Pubkey::find_program_address(
        &[
            KAMINO_LENDING_MARKET_AUTHORITY_SEED,
            KAMINO_MAIN_MARKET.as_ref(),
        ],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0;
    let collateral_mint = deterministic_pubkey(b"ask-2212-local-collateral-mint");
    let reserve_liquidity_supply = deterministic_pubkey(b"ask-2212-local-reserve-liquidity-supply");
    let vault_collateral_ata =
        get_associated_token_address_with_program_id(&vault, &collateral_mint, &spl_token::id());
    let mint_authority = Keypair::new_from_array(MINT_AUTHORITY_SEED).pubkey();
    let treasury = squads_test_treasury();
    let program_config = derive_squads_program_config();

    let mut reserve = Reserve::zeroed();
    reserve.lending_market = KAMINO_MAIN_MARKET;
    reserve.liquidity.mint_pubkey = USDC_MINT;
    reserve.liquidity.supply_vault = reserve_liquidity_supply;
    reserve.liquidity.total_available_amount = RESERVE_LIQUIDITY_RAW;
    reserve.liquidity.mint_decimals = u64::from(USDC_DECIMALS);
    reserve.liquidity.token_program = spl_token::id();
    reserve.collateral.mint_pubkey = collateral_mint;
    reserve.collateral.mint_total_supply = RESERVE_LIQUIDITY_RAW;
    reserve.collateral.supply_vault = vault_collateral_ata;

    let mut obligation_state = Obligation::zeroed();
    obligation_state.lending_market = KAMINO_MAIN_MARKET;
    obligation_state.owner = vault;
    obligation_state.deposits[0].deposit_reserve = KAMINO_MAIN_USDC_RESERVE;

    let files = [
        (
            "programConfig",
            write_account(
                &output_dir,
                "program-config",
                program_config,
                SQUADS_SMART_ACCOUNT_PROGRAM_ID,
                serialize_squads_program_config(treasury, treasury, 0),
            )?,
        ),
        (
            "usdcMint",
            write_account(
                &output_dir,
                "usdc-mint",
                USDC_MINT,
                spl_token::id(),
                mint_data(Some(mint_authority), 0, USDC_DECIMALS)?,
            )?,
        ),
        (
            "collateralMint",
            write_account(
                &output_dir,
                "collateral-mint",
                collateral_mint,
                spl_token::id(),
                mint_data(Some(market_authority), 0, USDC_DECIMALS)?,
            )?,
        ),
        (
            "reserveLiquiditySupply",
            write_account(
                &output_dir,
                "reserve-liquidity-supply",
                reserve_liquidity_supply,
                spl_token::id(),
                token_account_data(USDC_MINT, market_authority, RESERVE_LIQUIDITY_RAW)?,
            )?,
        ),
        (
            "vaultCollateralAta",
            write_account(
                &output_dir,
                "vault-collateral-ata",
                vault_collateral_ata,
                spl_token::id(),
                token_account_data(collateral_mint, vault, 0)?,
            )?,
        ),
        (
            "reserve",
            write_account(
                &output_dir,
                "reserve",
                KAMINO_MAIN_USDC_RESERVE,
                KAMINO_LEND_PROGRAM_ID,
                klend_data(&reserve),
            )?,
        ),
        (
            "obligation",
            write_account(
                &output_dir,
                "obligation",
                obligation,
                KAMINO_LEND_PROGRAM_ID,
                klend_data(&obligation_state),
            )?,
        ),
        (
            "market",
            write_account(
                &output_dir,
                "market",
                KAMINO_MAIN_MARKET,
                KAMINO_LEND_PROGRAM_ID,
                Vec::new(),
            )?,
        ),
    ];

    let manifest = serde_json::json!({
        "addresses": {
            "collateralMint": collateral_mint.to_string(),
            "market": KAMINO_MAIN_MARKET.to_string(),
            "marketAuthority": market_authority.to_string(),
            "mintAuthority": mint_authority.to_string(),
            "obligation": obligation.to_string(),
            "programConfig": program_config.to_string(),
            "reserve": KAMINO_MAIN_USDC_RESERVE.to_string(),
            "reserveLiquiditySupply": reserve_liquidity_supply.to_string(),
            "settings": settings.to_string(),
            "treasury": treasury.to_string(),
            "usdcMint": USDC_MINT.to_string(),
            "vault": vault.to_string(),
            "vaultCollateralAta": vault_collateral_ata.to_string(),
        },
        "files": files
            .into_iter()
            .map(|(name, path)| {
                (
                    name.to_owned(),
                    serde_json::Value::String(path.to_string_lossy().into_owned()),
                )
            })
            .collect::<serde_json::Map<_, _>>(),
    });
    fs::write(manifest_output, serde_json::to_vec_pretty(&manifest)?)?;
    println!("{treasury}");
    Ok(())
}
