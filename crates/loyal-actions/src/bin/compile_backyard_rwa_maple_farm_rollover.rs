//! Compile the exact forward-only Maple borrow/repay policy replacements.
//! This binary signs fresh PolicyCreate packets for review and simulation but
//! has no RPC or broadcast path.

use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use loyal_actions::{
    create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
    SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
};
use loyal_solana_env::solana_testing_keypair_from_env;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash, instruction::Instruction, pubkey::Pubkey, signature::Signer,
    transaction::Transaction,
};
use std::{fs, io::Read, str::FromStr};

const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const DELEGATE: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const MARKET: &str = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y";
const MARKET_AUTHORITY: &str = "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g";
const OBLIGATION: &str = "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT";
const DEBT_RESERVE: &str = "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo";
const DEBT_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const DEBT_SUPPLY: &str = "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM";
const DEBT_FEE_RECEIVER: &str = "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV";
const DEBT_CUSTODY: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
const DEBT_FARM: &str = "87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y";
const OBLIGATION_DEBT_FARM: &str = "CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr";
const FARMS_PROGRAM: &str = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const INSTRUCTIONS_SYSVAR: &str = "Sysvar1nstructions1111111111111111111111111";
const EXPECTED_SEED_BEFORE: u64 = 136;
const AMOUNT_CAP_RAW: u64 = 1_000_000_000_000;
const PACKET_LIMIT: usize = 1_232;

const BORROW_DISCRIMINATOR: [u8; 8] = [161, 128, 143, 245, 171, 199, 194, 6];
const REPAY_DISCRIMINATOR: [u8; 8] = [116, 174, 213, 76, 180, 53, 210, 144];

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Input {
    policy_seed_before: u64,
    settings_context_slot: u64,
    recent_blockhash: String,
    last_valid_block_height: u64,
    debt_farm: String,
    obligation_debt_farm: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Output {
    schema: &'static str,
    verdict: &'static str,
    broadcast: bool,
    cluster: &'static str,
    settings_context_slot: u64,
    policy_seed_before: u64,
    recent_blockhash: String,
    last_valid_block_height: u64,
    amount_cap_raw: u64,
    debt_farm: String,
    obligation_debt_farm: String,
    policies: Vec<CompiledPolicy>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct CompiledPolicy {
    operation: &'static str,
    seed: u64,
    policy: String,
    account_pubkeys: Vec<String>,
    discriminator_hex: String,
    create_instruction_data_sha256: String,
    packet_bytes: usize,
    transaction_base64: String,
    transaction_sha256: String,
    signature: String,
}

fn key(value: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(value).map_err(|error| error.to_string())
}

fn sha256(value: &[u8]) -> String {
    hex_bytes(&Sha256::digest(value))
}

fn hex_bytes(value: &[u8]) -> String {
    value.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn constraint(
    accounts: &[&str],
    discriminator: [u8; 8],
) -> Result<SemanticProgramInteractionConstraint, String> {
    Ok(SemanticProgramInteractionConstraint {
        program_id: key(KLEND)?,
        account_pubkeys: accounts
            .iter()
            .enumerate()
            .map(|(index, address)| Ok((index as u8, vec![key(address)?])))
            .collect::<Result<Vec<_>, String>>()?,
        account_data: Vec::new(),
        data: vec![
            SemanticProgramInteractionDataConstraint::SliceEquals {
                offset: 0,
                value: discriminator.to_vec(),
            },
            SemanticProgramInteractionDataConstraint::U64LessThanOrEqual {
                offset: 8,
                value: AMOUNT_CAP_RAW,
            },
        ],
    })
}

fn compile(
    operation: &'static str,
    seed: u64,
    accounts: &[&str],
    discriminator: [u8; 8],
    blockhash: Hash,
) -> Result<CompiledPolicy, String> {
    let signer = solana_testing_keypair_from_env().map_err(|error| error.to_string())?;
    if signer.pubkey() != key(AUTHORITY)? {
        return Err("SOLANA_TESTING_PK is not the pinned Settings authority".to_owned());
    }
    let instruction: Instruction = create_deployed_semantic_program_interaction_policy_instruction(
        key(SETTINGS)?,
        signer.pubkey(),
        key(DELEGATE)?,
        seed,
        0,
        vec![constraint(accounts, discriminator)?],
    )
    .map_err(|error| error.to_string())?;
    let transaction = Transaction::new_signed_with_payer(
        std::slice::from_ref(&instruction),
        Some(&signer.pubkey()),
        &[&signer],
        blockhash,
    );
    transaction.verify().map_err(|error| error.to_string())?;
    let wire = bincode::serialize(&transaction).map_err(|error| error.to_string())?;
    if wire.len() > PACKET_LIMIT {
        return Err(format!(
            "{operation} PolicyCreate packet is {} bytes",
            wire.len()
        ));
    }
    Ok(CompiledPolicy {
        operation,
        seed,
        policy: derive_action_account(&key(SETTINGS)?, seed).0.to_string(),
        account_pubkeys: accounts.iter().map(|value| (*value).to_owned()).collect(),
        discriminator_hex: hex_bytes(&discriminator),
        create_instruction_data_sha256: sha256(&instruction.data),
        packet_bytes: wire.len(),
        transaction_base64: BASE64.encode(&wire),
        transaction_sha256: sha256(&wire),
        signature: transaction.signatures[0].to_string(),
    })
}

fn run() -> Result<Output, String> {
    let args = std::env::args().skip(1).collect::<Vec<_>>();
    let mut source = if let Some(input_path) = args.first() {
        fs::read_to_string(input_path).map_err(|error| error.to_string())?
    } else {
        String::new()
    };
    if args.is_empty() {
        std::io::stdin()
            .read_to_string(&mut source)
            .map_err(|error| error.to_string())?;
    }
    let input: Input = serde_json::from_str(&source).map_err(|error| error.to_string())?;
    if input.policy_seed_before != EXPECTED_SEED_BEFORE
        || input.settings_context_slot == 0
        || input.last_valid_block_height == 0
        || input.debt_farm != DEBT_FARM
        || input.obligation_debt_farm != OBLIGATION_DEBT_FARM
    {
        return Err("finalized Settings or Maple farm topology drifted".to_owned());
    }
    let blockhash = Hash::from_str(&input.recent_blockhash).map_err(|error| error.to_string())?;
    let borrow_accounts = [
        VAULT,
        OBLIGATION,
        MARKET,
        MARKET_AUTHORITY,
        DEBT_RESERVE,
        DEBT_MINT,
        DEBT_SUPPLY,
        DEBT_FEE_RECEIVER,
        DEBT_CUSTODY,
        KLEND,
        TOKEN_PROGRAM,
        INSTRUCTIONS_SYSVAR,
        OBLIGATION_DEBT_FARM,
        DEBT_FARM,
        FARMS_PROGRAM,
    ];
    let repay_accounts = [
        VAULT,
        OBLIGATION,
        MARKET,
        DEBT_RESERVE,
        DEBT_MINT,
        DEBT_SUPPLY,
        DEBT_CUSTODY,
        TOKEN_PROGRAM,
        INSTRUCTIONS_SYSVAR,
        OBLIGATION_DEBT_FARM,
        DEBT_FARM,
        MARKET_AUTHORITY,
        FARMS_PROGRAM,
    ];
    let policies = vec![
        compile(
            "borrow",
            137,
            &borrow_accounts,
            BORROW_DISCRIMINATOR,
            blockhash,
        )?,
        compile(
            "repay",
            138,
            &repay_accounts,
            REPAY_DISCRIMINATOR,
            blockhash,
        )?,
    ];
    Ok(Output {
        schema: "loyal-backyard-rwa-maple-farm-rollover/v1",
        verdict: "SIGNED_UNSENT_POLICY_CREATE_PAIR",
        broadcast: false,
        cluster: "mainnet-beta",
        settings_context_slot: input.settings_context_slot,
        policy_seed_before: input.policy_seed_before,
        recent_blockhash: input.recent_blockhash,
        last_valid_block_height: input.last_valid_block_height,
        amount_cap_raw: AMOUNT_CAP_RAW,
        debt_farm: input.debt_farm,
        obligation_debt_farm: input.obligation_debt_farm,
        policies,
    })
}

fn main() {
    match run() {
        Ok(output) => {
            let encoded = format!("{}\n", serde_json::to_string_pretty(&output).unwrap());
            if let Some(output_path) = std::env::args().nth(2) {
                fs::write(output_path, encoded).unwrap();
            } else {
                print!("{encoded}");
            }
        }
        Err(error) => {
            eprintln!("{error}");
            std::process::exit(1);
        }
    }
}
