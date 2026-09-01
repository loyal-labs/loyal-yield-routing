use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_actions::autonomous_vaults::{
    create_voltr_custom_policies, VoltrCustomPolicyIdentity, VoltrCustomPolicySeeds,
    VoltrCustomPolicyTemplates,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::{io::Read, str::FromStr};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Input {
    identity: Identity,
    instructions: Templates,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Identity {
    settings: String,
    authority: String,
    delegated_signer: String,
    manager: String,
    squads_program: String,
    vault_index: u8,
    vault: String,
    strategy: String,
    voltr_program: String,
    adaptor_program: String,
    token_program: String,
    asset_mint: String,
    squads_asset_ata: String,
    strategy_asset_ata: String,
    report_ticket: String,
    max_amount_raw: String,
    asset_decimals: u8,
    seeds: Seeds,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Seeds {
    allocation: String,
    nav_refresh: String,
    stage_withdrawal: String,
    withdraw: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Templates {
    allocation_arm: WireInstruction,
    allocation: WireInstruction,
    nav_refresh_arm: WireInstruction,
    nav_refresh: WireInstruction,
    stage_withdrawal: WireInstruction,
    withdraw_arm: WireInstruction,
    withdraw: WireInstruction,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct WireInstruction {
    program_id: String,
    accounts: Vec<WireAccount>,
    data_base64: String,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct WireAccount {
    address: String,
    signer: bool,
    writable: bool,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct OutputPolicy {
    operation: &'static str,
    seed: String,
    policy: String,
    constraint_index: u8,
    constraint_indices: Vec<u8>,
    create_instruction: WireInstruction,
    replace_instruction: WireInstruction,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Output {
    schema: &'static str,
    verdict: &'static str,
    source_sha256: String,
    physical_policy_count: u8,
    deployment_ready: bool,
    policies: Vec<OutputPolicy>,
}

fn pubkey(value: &str, label: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(value).map_err(|error| format!("invalid {label}: {error}"))
}

fn u64_value(value: &str, label: &str) -> Result<u64, String> {
    value
        .parse::<u64>()
        .map_err(|error| format!("invalid {label}: {error}"))
}

fn instruction(value: WireInstruction) -> Result<Instruction, String> {
    Ok(Instruction {
        program_id: pubkey(&value.program_id, "instruction program")?,
        accounts: value
            .accounts
            .into_iter()
            .map(|account| {
                let key = pubkey(&account.address, "instruction account")?;
                Ok(match (account.writable, account.signer) {
                    (true, true) => AccountMeta::new(key, true),
                    (true, false) => AccountMeta::new(key, false),
                    (false, true) => AccountMeta::new_readonly(key, true),
                    (false, false) => AccountMeta::new_readonly(key, false),
                })
            })
            .collect::<Result<Vec<_>, String>>()?,
        data: BASE64
            .decode(value.data_base64)
            .map_err(|error| format!("invalid instruction base64: {error}"))?,
    })
}

fn wire(instruction: Instruction) -> WireInstruction {
    WireInstruction {
        program_id: instruction.program_id.to_string(),
        accounts: instruction
            .accounts
            .into_iter()
            .map(|account| WireAccount {
                address: account.pubkey.to_string(),
                signer: account.is_signer,
                writable: account.is_writable,
            })
            .collect(),
        data_base64: BASE64.encode(instruction.data),
    }
}

fn run() -> Result<(), String> {
    let mut input_bytes = Vec::new();
    std::io::stdin()
        .read_to_end(&mut input_bytes)
        .map_err(|error| error.to_string())?;
    let input: Input = serde_json::from_slice(&input_bytes)
        .map_err(|error| format!("invalid compiler input: {error}"))?;
    let source_sha256 = format!("{:x}", Sha256::digest(&input_bytes));
    let identity = VoltrCustomPolicyIdentity {
        settings: pubkey(&input.identity.settings, "Settings")?,
        authority: pubkey(&input.identity.authority, "authority")?,
        delegated_signer: pubkey(&input.identity.delegated_signer, "delegated signer")?,
        manager: pubkey(&input.identity.manager, "manager")?,
        squads_program: pubkey(&input.identity.squads_program, "Squads program")?,
        vault_index: input.identity.vault_index,
        vault: pubkey(&input.identity.vault, "Voltr vault")?,
        strategy: pubkey(&input.identity.strategy, "strategy")?,
        voltr_program: pubkey(&input.identity.voltr_program, "Voltr program")?,
        adaptor_program: pubkey(&input.identity.adaptor_program, "adaptor program")?,
        token_program: pubkey(&input.identity.token_program, "token program")?,
        asset_mint: pubkey(&input.identity.asset_mint, "asset mint")?,
        squads_asset_ata: pubkey(&input.identity.squads_asset_ata, "Squads asset ATA")?,
        strategy_asset_ata: pubkey(&input.identity.strategy_asset_ata, "strategy asset ATA")?,
        report_ticket: pubkey(&input.identity.report_ticket, "report ticket")?,
        max_amount_raw: u64_value(&input.identity.max_amount_raw, "maximum amount")?,
        asset_decimals: input.identity.asset_decimals,
        seeds: VoltrCustomPolicySeeds {
            allocation: u64_value(&input.identity.seeds.allocation, "allocation seed")?,
            nav_refresh: u64_value(&input.identity.seeds.nav_refresh, "NAV refresh seed")?,
            stage_withdrawal: u64_value(
                &input.identity.seeds.stage_withdrawal,
                "withdrawal staging seed",
            )?,
            withdraw: u64_value(&input.identity.seeds.withdraw, "withdraw seed")?,
        },
    };
    let templates = VoltrCustomPolicyTemplates {
        allocation_arm: instruction(input.instructions.allocation_arm)?,
        allocation: instruction(input.instructions.allocation)?,
        nav_refresh_arm: instruction(input.instructions.nav_refresh_arm)?,
        nav_refresh: instruction(input.instructions.nav_refresh)?,
        stage_withdrawal: instruction(input.instructions.stage_withdrawal)?,
        withdraw_arm: instruction(input.instructions.withdraw_arm)?,
        withdraw: instruction(input.instructions.withdraw)?,
    };
    let policies =
        create_voltr_custom_policies(&identity, &templates).map_err(|error| error.to_string())?;
    let entries = [
        ("allocation", policies.allocation),
        ("nav-refresh", policies.nav_refresh),
        ("stage-withdrawal", policies.stage_withdrawal),
        ("withdraw", policies.withdraw),
    ]
    .into_iter()
    .map(|(operation, policy)| OutputPolicy {
        operation,
        seed: policy.seed.to_string(),
        policy: policy.policy.to_string(),
        constraint_index: policy.constraint_index,
        constraint_indices: policy.constraint_indices,
        create_instruction: wire(policy.create_instruction),
        replace_instruction: wire(policy.replace_instruction),
    })
    .collect();
    println!(
        "{}",
        serde_json::to_string(&Output {
            schema: "loyal-voltr-custom-policy-artifact/v3",
            verdict: "VOLTR_CUSTOM_POLICY_ARTIFACT_COMPILED_NOT_DEPLOYED",
            source_sha256,
            physical_policy_count: 4,
            deployment_ready: false,
            policies: entries,
        })
        .map_err(|error| error.to_string())?
    );
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
