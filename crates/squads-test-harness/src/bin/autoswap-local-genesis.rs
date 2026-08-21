use std::{env, fs, path::PathBuf};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use squads_test_harness::{
    derive_squads_program_config, serialize_squads_program_config, squads_test_treasury,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let output = env::args_os()
        .nth(1)
        .map(PathBuf::from)
        .ok_or("output path is required")?;
    let authority = squads_test_treasury();
    let data = serialize_squads_program_config(authority, authority, 0);
    let program_config = derive_squads_program_config();
    let account = serde_json::json!({
        "account": {
            "data": [BASE64_STANDARD.encode(data), "base64"],
            "executable": false,
            "lamports": 1_000_000_000u64,
            "owner": SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string(),
            "rentEpoch": 0,
            "space": 160,
        },
        "pubkey": program_config.to_string(),
    });
    fs::write(&output, serde_json::to_vec_pretty(&account)?)?;
    println!("{program_config} {authority}");
    Ok(())
}
