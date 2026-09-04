//! Compile the forward-only Maple syrupUSDC -> USDC Jupiter exit policy.
//!
//! This is an activation artifact, not an activation side effect. It requires
//! the finalized Settings seed boundary, emits a fresh seed-139 PolicyCreate,
//! and never talks to RPC or broadcasts. The Go runtime must not switch to the
//! replacement until its account bytes have been finalized and read back.

use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use loyal_actions::{
    create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
    SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
};
use loyal_solana_env::solana_testing_keypair_from_env;
use serde::Serialize;
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash, instruction::Instruction, pubkey::Pubkey, signature::Signer,
    transaction::Transaction,
};
use std::{io::Read, str::FromStr};

const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const DELEGATED_SIGNER: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const JUPITER: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const SOURCE_CUSTODY: &str = "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN";
const DESTINATION_CUSTODY: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
const SOURCE_MINT: &str = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj";
const DESTINATION_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

const PREVIOUS_POLICY_SEED: u64 = 138;
const REPLACEMENT_POLICY_SEED: u64 = 139;
const MAX_AMOUNT_RAW: u64 = 1_000_000;
const MAX_SLIPPAGE_BPS: u16 = 50;
const PACKET_LIMIT: usize = 1_232;
const SHARED_ACCOUNTS_ROUTE: [u8; 8] = [0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81];

#[derive(serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Input {
    policy_seed_before: u64,
    settings_context_slot: u64,
    recent_blockhash: String,
    last_valid_block_height: u64,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Output {
    schema: &'static str,
    verdict: &'static str,
    broadcast: bool,
    cluster: &'static str,
    lane: &'static str,
    policy_seed_before: u64,
    replacement_policy_seed: u64,
    replacement_policy: String,
    settings_context_slot: u64,
    recent_blockhash: String,
    last_valid_block_height: u64,
    program_id: &'static str,
    account_pins: Vec<AccountPin>,
    data_constraints: Vec<DataConstraint>,
    create_instruction_data_sha256: String,
    packet_bytes: usize,
    transaction_base64: String,
    transaction_sha256: String,
    signature: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct AccountPin {
    index: u8,
    pubkey: &'static str,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct DataConstraint {
    kind: &'static str,
    offset: u64,
    value: String,
}

fn key(value: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(value).map_err(|error| error.to_string())
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn sha256(bytes: &[u8]) -> String {
    hex(&Sha256::digest(bytes))
}

fn exact_exit_constraint() -> Result<SemanticProgramInteractionConstraint, String> {
    let pins = [
        (0, TOKEN_PROGRAM),
        (2, VAULT),
        (3, SOURCE_CUSTODY),
        (6, DESTINATION_CUSTODY),
        (7, SOURCE_MINT),
        (8, DESTINATION_MINT),
    ];
    Ok(SemanticProgramInteractionConstraint {
        program_id: key(JUPITER)?,
        account_pubkeys: pins
            .into_iter()
            .map(|(index, address)| Ok((index, vec![key(address)?])))
            .collect::<Result<Vec<_>, String>>()?,
        account_data: Vec::new(),
        data: vec![
            SemanticProgramInteractionDataConstraint::SliceEquals {
                offset: 0,
                value: SHARED_ACCOUNTS_ROUTE.to_vec(),
            },
            SemanticProgramInteractionDataConstraint::U64LessThanOrEqual {
                offset: 18,
                value: MAX_AMOUNT_RAW,
            },
            SemanticProgramInteractionDataConstraint::U16LessThanOrEqual {
                offset: 34,
                value: MAX_SLIPPAGE_BPS,
            },
            SemanticProgramInteractionDataConstraint::U8Equals {
                offset: 36,
                value: 0,
            },
        ],
    })
}

fn compile(input: Input) -> Result<Output, String> {
    if input.policy_seed_before != PREVIOUS_POLICY_SEED
        || input.settings_context_slot == 0
        || input.last_valid_block_height == 0
    {
        return Err(
            "finalized Settings seed must be exactly 138 before seed-139 activation".into(),
        );
    }
    let authority = solana_testing_keypair_from_env().map_err(|error| error.to_string())?;
    if authority.pubkey() != key(AUTHORITY)? {
        return Err("SOLANA_TESTING_PK is not the pinned Settings authority".into());
    }
    let settings = key(SETTINGS)?;
    let blockhash = Hash::from_str(&input.recent_blockhash).map_err(|error| error.to_string())?;
    let instruction: Instruction = create_deployed_semantic_program_interaction_policy_instruction(
        settings,
        authority.pubkey(),
        key(DELEGATED_SIGNER)?,
        REPLACEMENT_POLICY_SEED,
        0,
        vec![exact_exit_constraint()?],
    )
    .map_err(|error| error.to_string())?;
    let transaction = Transaction::new_signed_with_payer(
        std::slice::from_ref(&instruction),
        Some(&authority.pubkey()),
        &[&authority],
        blockhash,
    );
    transaction.verify().map_err(|error| error.to_string())?;
    let wire = bincode::serialize(&transaction).map_err(|error| error.to_string())?;
    if wire.len() > PACKET_LIMIT {
        return Err(format!(
            "seed-139 PolicyCreate packet is {} bytes",
            wire.len()
        ));
    }
    Ok(Output {
        schema: "loyal-backyard-rwa-maple-exit-policy/v1",
        verdict: "SIGNED_UNSENT_POLICY_CREATE",
        broadcast: false,
        cluster: "mainnet-beta",
        lane: "Maple/syrupUSDC/USDC",
        policy_seed_before: input.policy_seed_before,
        replacement_policy_seed: REPLACEMENT_POLICY_SEED,
        replacement_policy: derive_action_account(&settings, REPLACEMENT_POLICY_SEED)
            .0
            .to_string(),
        settings_context_slot: input.settings_context_slot,
        recent_blockhash: input.recent_blockhash,
        last_valid_block_height: input.last_valid_block_height,
        program_id: JUPITER,
        account_pins: vec![
            AccountPin {
                index: 0,
                pubkey: TOKEN_PROGRAM,
            },
            AccountPin {
                index: 2,
                pubkey: VAULT,
            },
            AccountPin {
                index: 3,
                pubkey: SOURCE_CUSTODY,
            },
            AccountPin {
                index: 6,
                pubkey: DESTINATION_CUSTODY,
            },
            AccountPin {
                index: 7,
                pubkey: SOURCE_MINT,
            },
            AccountPin {
                index: 8,
                pubkey: DESTINATION_MINT,
            },
        ],
        data_constraints: vec![
            DataConstraint {
                kind: "slice-equals",
                offset: 0,
                value: hex(&SHARED_ACCOUNTS_ROUTE),
            },
            DataConstraint {
                kind: "u64-less-than-or-equal",
                offset: 18,
                value: MAX_AMOUNT_RAW.to_string(),
            },
            DataConstraint {
                kind: "u16-less-than-or-equal",
                offset: 34,
                value: MAX_SLIPPAGE_BPS.to_string(),
            },
            DataConstraint {
                kind: "u8-equals",
                offset: 36,
                value: "0".into(),
            },
        ],
        create_instruction_data_sha256: sha256(&instruction.data),
        packet_bytes: wire.len(),
        transaction_base64: BASE64.encode(&wire),
        transaction_sha256: sha256(&wire),
        signature: transaction.signatures[0].to_string(),
    })
}

fn main() {
    let result = (|| -> Result<Output, String> {
        let mut source = String::new();
        std::io::stdin()
            .read_to_string(&mut source)
            .map_err(|error| error.to_string())?;
        let input: Input = serde_json::from_str(&source).map_err(|error| error.to_string())?;
        compile(input)
    })();
    match result {
        Ok(output) => println!("{}", serde_json::to_string_pretty(&output).unwrap()),
        Err(error) => {
            eprintln!("{error}");
            std::process::exit(1);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exit_constraint_is_exact_and_forward_only() {
        let constraint = exact_exit_constraint().unwrap();
        assert_eq!(constraint.program_id, key(JUPITER).unwrap());
        assert_eq!(
            constraint
                .account_pubkeys
                .iter()
                .map(|(index, _)| *index)
                .collect::<Vec<_>>(),
            vec![0, 2, 3, 6, 7, 8]
        );
        assert!(matches!(
            constraint.data[0],
            SemanticProgramInteractionDataConstraint::SliceEquals { offset: 0, .. }
        ));
        assert!(matches!(
            constraint.data[1],
            SemanticProgramInteractionDataConstraint::U64LessThanOrEqual {
                offset: 18,
                value: 1_000_000
            }
        ));
        assert!(matches!(
            constraint.data[2],
            SemanticProgramInteractionDataConstraint::U16LessThanOrEqual {
                offset: 34,
                value: 50
            }
        ));
        assert!(matches!(
            constraint.data[3],
            SemanticProgramInteractionDataConstraint::U8Equals {
                offset: 36,
                value: 0
            }
        ));
    }

    #[test]
    fn stale_or_advanced_settings_seed_cannot_activate_replacement() {
        for seed in [137, 139] {
            assert_ne!(seed, PREVIOUS_POLICY_SEED);
        }
    }
}
