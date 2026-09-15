//! Compile the owner-approved four-policy Backyard RWA basic set.
//!
//! This binary is deterministic and offline. It uses the fixed seven-strategy
//! topology in the SDK, does not read RPC, and never signs or broadcasts.

use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use loyal_actions::backyard_basic_policy_set::compile_backyard_basic_policy_set;
use serde_json::json;
use solana_sdk::pubkey::Pubkey;
use std::{error::Error, fs, path::PathBuf, str::FromStr};

const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const DELEGATE: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const POLICY_SEED_BEFORE: &str = "140";
const POLICY_SEED_BASE: u64 = 141;

fn main() -> Result<(), Box<dyn Error>> {
    let settings = Pubkey::from_str(SETTINGS)?;
    let authority = Pubkey::from_str(AUTHORITY)?;
    let delegate = Pubkey::from_str(DELEGATE)?;
    let plans = compile_backyard_basic_policy_set(settings, authority, delegate, POLICY_SEED_BASE)?;
    if plans.len() != 4
        || plans.iter().map(|plan| plan.seed).collect::<Vec<_>>() != [141, 142, 143, 144]
    {
        return Err("basic policy compiler did not produce seeds 141-144".into());
    }
    if plans.iter().any(|plan| plan.signed_packet_bytes > 1232) {
        return Err("basic policy compiler produced an oversized legacy packet".into());
    }

    let policies = plans
        .iter()
        .map(|plan| {
            json!({
                "seed": plan.seed.to_string(),
                "account": plan.account.to_string(),
                "family": plan.family,
                "constraints": plan.summary,
                "instruction": {
                    "programId": plan.instruction.program_id.to_string(),
                    "accounts": plan.instruction.accounts.iter().map(|account| json!({
                        "address": account.pubkey.to_string(),
                        "signer": account.is_signer,
                        "writable": account.is_writable,
                    })).collect::<Vec<_>>(),
                    "dataBase64": BASE64.encode(&plan.instruction.data),
                },
                "dataSha256": plan.data_sha256,
                "legacyPacketBytes": plan.signed_packet_bytes,
            })
        })
        .collect::<Vec<_>>();
    let artifact = json!({
        "schema": "loyal-backyard-rwa-basic-policy-artifact/v1",
        "settings": settings.to_string(),
        "authority": authority.to_string(),
        "delegate": delegate.to_string(),
        "policySeedBefore": POLICY_SEED_BEFORE,
        "policies": policies,
    });

    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..");
    let output = root.join("docs/evidence/backyard-rwa-basic/policy-artifact-v1.json");
    if let Some(parent) = output.parent() {
        fs::create_dir_all(parent)?;
    }
    fs::write(
        &output,
        format!("{}\n", serde_json::to_string_pretty(&artifact)?),
    )?;
    println!(
        "{}",
        serde_json::json!({
            "wrote": output,
            "broadcast": false,
            "seeds": ["141", "142", "143", "144"],
            "legacyPacketBytes": plans.iter().map(|plan| plan.signed_packet_bytes).collect::<Vec<_>>(),
        })
    );
    Ok(())
}
