use std::{env, fs, path::PathBuf};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use spl_token::{
    solana_program::{program_option::COption, program_pack::Pack},
    state::Mint,
};
use squads_test_harness::{
    derive_squads_program_config, serialize_squads_program_config, squads_test_treasury,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_DECIMALS, USDC_MINT,
};

fn write_account(
    output: &PathBuf,
    pubkey: impl ToString,
    owner: impl ToString,
    data: Vec<u8>,
) -> Result<(), Box<dyn std::error::Error>> {
    let account = serde_json::json!({
        "account": {
            "data": [BASE64_STANDARD.encode(&data), "base64"],
            "executable": false,
            "lamports": 1_000_000_000u64,
            "owner": owner.to_string(),
            "rentEpoch": 0,
            "space": data.len(),
        },
        "pubkey": pubkey.to_string(),
    });
    fs::write(output, serde_json::to_vec_pretty(&account)?)?;
    Ok(())
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let program_config_output = env::args_os()
        .nth(1)
        .map(PathBuf::from)
        .ok_or("program config output path is required")?;
    let usdc_mint_output = env::args_os()
        .nth(2)
        .map(PathBuf::from)
        .ok_or("USDC mint output path is required")?;
    let authority = squads_test_treasury();
    let program_config = derive_squads_program_config();
    write_account(
        &program_config_output,
        program_config,
        SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        serialize_squads_program_config(authority, authority, 0),
    )?;

    let mut mint_data = vec![0; Mint::LEN];
    Mint::pack(
        Mint {
            mint_authority: COption::None,
            supply: 0,
            decimals: USDC_DECIMALS,
            is_initialized: true,
            freeze_authority: COption::None,
        },
        &mut mint_data,
    )?;
    write_account(&usdc_mint_output, USDC_MINT, spl_token::id(), mint_data)?;

    println!("{program_config} {authority} {USDC_MINT}");
    Ok(())
}
