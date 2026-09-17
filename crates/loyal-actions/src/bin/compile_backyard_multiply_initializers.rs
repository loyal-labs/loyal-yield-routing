//! Offline installation artifact for the three pilot obligation initializers.
//! The caller supplies finalized Settings.policy_seed; this does not establish
//! installation or activation and never signs, reads secrets, or sends.
use base64::{engine::general_purpose::STANDARD, Engine};
use loyal_actions::{
    backyard_multiply_initializer::{
        backyard_multiply_initializers, compile_backyard_multiply_initializer_policies,
    },
    derive_action_account,
};
use serde_json::json;
use solana_sdk::{message::Message, pubkey::Pubkey};
use std::{error::Error, str::FromStr};

fn main() -> Result<(), Box<dyn Error>> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    if args.len() != 2 || args[0] != "--policy-seed-before" {
        return Err("usage: compile-backyard-multiply-initializers --policy-seed-before <finalized Settings policy_seed>".into());
    }
    let before: u64 = args[1].parse()?;
    let first = before.checked_add(1).ok_or("policy seed overflow")?;
    let settings = Pubkey::from_str("5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6")?;
    let authority = Pubkey::from_str("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ")?;
    let delegate = Pubkey::from_str("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5")?;
    let initializers = backyard_multiply_initializers(settings)?;
    let installs =
        compile_backyard_multiply_initializer_policies(settings, authority, delegate, first)?;
    let mut policies = Vec::new();
    for (index, (init, install)) in initializers.iter().zip(installs.iter()).enumerate() {
        let seed = first
            .checked_add(index as u64)
            .ok_or("policy seed overflow")?;
        let packet_bytes =
            bincode::serialize(&Message::new(&[install.clone()], Some(&authority)))?.len() + 65;
        if packet_bytes > 1232 {
            return Err("initializer installation exceeds packet limit".into());
        }
        policies.push(json!({
            "lane": init.lane, "seed": seed.to_string(),
            "account": derive_action_account(&settings, seed).0.to_string(),
            "obligation": init.instruction.accounts[2].pubkey.to_string(),
            "legacyPacketBytes": packet_bytes,
            "instruction": {
                "programId": install.program_id.to_string(),
                "accounts": install.accounts.iter().map(|a| json!({"address":a.pubkey.to_string(),"signer":a.is_signer,"writable":a.is_writable})).collect::<Vec<_>>(),
                "dataBase64": STANDARD.encode(&install.data),
            }
        }));
    }
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "schema":"loyal-backyard-multiply-initializer-install/v1", "broadcast":false,
            "proofLevel":"OFFLINE_COMPILER_ARTIFACT_NOT_INSTALLED_AUTHORITY",
            "settings":settings.to_string(),"authority":authority.to_string(),"delegate":delegate.to_string(),
            "policySeedBefore":before.to_string(),"policies":policies,
        }))?
    );
    Ok(())
}
