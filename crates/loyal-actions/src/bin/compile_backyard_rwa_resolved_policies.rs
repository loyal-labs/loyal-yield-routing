//! Compile the resolved 14-policy Backyard RWA catalog into exact Squads
//! PolicyCreate and PolicyUpdate instructions. Discovery stays outside this
//! binary: it accepts only a resolver artifact whose lane graph and Jupiter
//! headers are explicitly marked resolved, and fails closed on any incomplete
//! or reordered policy family.

use base64::{engine::general_purpose::STANDARD as BASE64, Engine};
use loyal_actions::{
    create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
    update_semantic_program_interaction_policy_instruction, SemanticProgramInteractionConstraint,
    SemanticProgramInteractionDataConstraint,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    instruction::Instruction, message::Message, pubkey::Pubkey, signature::Signature,
    transaction::Transaction,
};
use std::{io::Read, str::FromStr};

const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const DELEGATED_SIGNER: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const POLICY_SEED_BEFORE: u64 = 56;
const PACKET_LIMIT: usize = 1_232;
const EXPECTED_NAMES: [&str; 14] = [
    "lane/OnRe/ONyc/USDC",
    "lane/OnRe/ONyc/USDG",
    "lane/OnRe/ONyc/USDS",
    "lane/Prime/PRIME/USDC",
    "lane/Prime/PRIME/PYUSD",
    "lane/Prime/PRIME/USDS",
    "lane/Maple/syrupUSDC/USDC",
    "lane/Maple/syrupUSDC/USDG",
    "lane/Maple/syrupUSDC/PYUSD",
    "lane/AUTO/AUTO/PYUSD",
    "lane/Ethena/USDe/PYUSD",
    "swap/stable-to-rwa",
    "swap/rwa-to-stable",
    "swap/stable-to-stable",
];
const PHASE_ONE_NAMES: [&str; 5] = [
    "lane/Prime/PRIME/USDC/deposit",
    "lane/Prime/PRIME/USDC/borrow",
    "lane/Prime/PRIME/USDC/repay",
    "lane/Prime/PRIME/USDC/withdraw",
    "swap/Prime/PRIME/USDC",
];

#[derive(Clone, Copy, PartialEq, Eq)]
enum CompileMode {
    PhaseOne,
    PhaseTwo,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Input {
    schema: String,
    addresses_resolved: bool,
    swap_headers_resolved: bool,
    catalog_sha256: String,
    resolution_sha256: String,
    settings: String,
    authority: String,
    delegated_signer: String,
    account_index: u8,
    policy_seed_before: String,
    policies: Vec<PolicyInput>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct PolicyInput {
    name: String,
    semantic_edge_count: u16,
    constraints: Vec<ConstraintInput>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct ConstraintInput {
    program_id: String,
    account_pubkeys: Vec<AccountConstraintInput>,
    data: Vec<DataConstraintInput>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct AccountConstraintInput {
    index: u8,
    pubkeys: Vec<String>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(tag = "kind", rename_all = "kebab-case")]
enum DataConstraintInput {
    SliceEquals {
        offset: u64,
        #[serde(rename = "valueHex")]
        value_hex: String,
    },
    U8Equals {
        offset: u64,
        value: u8,
    },
    U16Equals {
        offset: u64,
        value: u16,
    },
    U16LessThanOrEqual {
        offset: u64,
        value: u16,
    },
    U32Equals {
        offset: u64,
        value: u32,
    },
    U64LessThanOrEqual {
        offset: u64,
        value: u64,
    },
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Output {
    schema: &'static str,
    phase: &'static str,
    verdict: &'static str,
    broadcast: bool,
    physical_policy_count: usize,
    policy_seed_before: String,
    catalog_sha256: String,
    resolution_sha256: String,
    source_sha256: String,
    policies: Vec<PolicyOutput>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct PolicyOutput {
    name: String,
    seed: String,
    policy: String,
    semantic_edge_count: u16,
    constraint_count: usize,
    constraints: Vec<ConstraintInput>,
    create_packet_bytes: usize,
    update_packet_bytes: usize,
    create_instruction: WireInstruction,
    update_instruction: WireInstruction,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct WireInstruction {
    program_id: String,
    accounts: Vec<WireAccount>,
    data_base64: String,
    data_sha256: String,
}

#[derive(Serialize)]
struct WireAccount {
    address: String,
    signer: bool,
    writable: bool,
}

fn key(value: &str, label: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(value).map_err(|_| format!("{label} is not a public key"))
}

fn hex(value: &str) -> Result<Vec<u8>, String> {
    if value.is_empty()
        || value.len() % 2 != 0
        || !value.bytes().all(|byte| byte.is_ascii_hexdigit())
    {
        return Err("slice-equals valueHex must be non-empty even-length hex".to_owned());
    }
    (0..value.len())
        .step_by(2)
        .map(|index| {
            u8::from_str_radix(&value[index..index + 2], 16).map_err(|error| error.to_string())
        })
        .collect()
}

fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn semantic(input: ConstraintInput) -> Result<SemanticProgramInteractionConstraint, String> {
    let mut seen = std::collections::BTreeSet::new();
    let account_pubkeys = input
        .account_pubkeys
        .into_iter()
        .map(|constraint| {
            if constraint.pubkeys.is_empty() || !seen.insert(constraint.index) {
                return Err(
                    "account constraints must have unique indexes and non-empty keys".to_owned(),
                );
            }
            Ok((
                constraint.index,
                constraint
                    .pubkeys
                    .iter()
                    .map(|value| key(value, "account constraint"))
                    .collect::<Result<Vec<_>, _>>()?,
            ))
        })
        .collect::<Result<Vec<_>, String>>()?;
    let data = input
        .data
        .into_iter()
        .map(|constraint| match constraint {
            DataConstraintInput::SliceEquals { offset, value_hex } => {
                Ok(SemanticProgramInteractionDataConstraint::SliceEquals {
                    offset,
                    value: hex(&value_hex)?,
                })
            }
            DataConstraintInput::U8Equals { offset, value } => {
                Ok(SemanticProgramInteractionDataConstraint::U8Equals { offset, value })
            }
            DataConstraintInput::U16Equals { offset, value } => {
                Ok(SemanticProgramInteractionDataConstraint::U16Equals { offset, value })
            }
            DataConstraintInput::U16LessThanOrEqual { offset, value } => {
                Ok(SemanticProgramInteractionDataConstraint::U16LessThanOrEqual { offset, value })
            }
            DataConstraintInput::U32Equals { offset, value } => {
                Ok(SemanticProgramInteractionDataConstraint::U32Equals { offset, value })
            }
            DataConstraintInput::U64LessThanOrEqual { offset, value } => {
                Ok(SemanticProgramInteractionDataConstraint::U64LessThanOrEqual { offset, value })
            }
        })
        .collect::<Result<Vec<_>, String>>()?;
    if data.is_empty() {
        return Err("every instruction constraint must pin instruction data".to_owned());
    }
    Ok(SemanticProgramInteractionConstraint {
        program_id: key(&input.program_id, "constraint program")?,
        account_pubkeys,
        account_data: Vec::new(),
        data,
    })
}

fn wire(instruction: &Instruction) -> WireInstruction {
    WireInstruction {
        program_id: instruction.program_id.to_string(),
        accounts: instruction
            .accounts
            .iter()
            .map(|account| WireAccount {
                address: account.pubkey.to_string(),
                signer: account.is_signer,
                writable: account.is_writable,
            })
            .collect(),
        data_base64: BASE64.encode(&instruction.data),
        data_sha256: sha256(&instruction.data),
    }
}

/// Full wire size with the message's actual number of 64-byte signature slots.
/// Signature contents cannot affect packet size, so this needs no private key.
fn packet_bytes(instruction: &Instruction, fee_payer: Pubkey) -> Result<usize, String> {
    let message = Message::new(std::slice::from_ref(instruction), Some(&fee_payer));
    let signatures = vec![Signature::default(); message.header.num_required_signatures as usize];
    bincode::serialize(&Transaction {
        signatures,
        message,
    })
    .map(|bytes| bytes.len())
    .map_err(|error| error.to_string())
}

fn compile(input: Input, source_sha256: String, mode: CompileMode) -> Result<Output, String> {
    let (phase, expected_names): (&str, &[&str]) = match mode {
        CompileMode::PhaseOne => ("phase1", &PHASE_ONE_NAMES),
        CompileMode::PhaseTwo => ("phase2", &EXPECTED_NAMES),
    };
    let policy_seed_before = input
        .policy_seed_before
        .parse::<u64>()
        .map_err(|_| "policySeedBefore is not a u64".to_owned())?;
    if input.schema != "loyal-backyard-rwa-policy-compiler-input/v1"
        || !input.addresses_resolved
        || !input.swap_headers_resolved
        || input.catalog_sha256.len() != 64
        || input.resolution_sha256.len() != 64
        || input.settings != SETTINGS
        || input.authority != AUTHORITY
        || input.delegated_signer != DELEGATED_SIGNER
        || input.account_index != 0
        || (mode == CompileMode::PhaseTwo && policy_seed_before != POLICY_SEED_BEFORE)
        || input.policies.len() != expected_names.len()
    {
        return Err(
            "resolved compiler identity/readiness boundary is incomplete or drifted".to_owned(),
        );
    }
    let settings = key(&input.settings, "settings")?;
    let authority = key(&input.authority, "authority")?;
    let delegated_signer = key(&input.delegated_signer, "delegated signer")?;
    let mut policies = Vec::with_capacity(expected_names.len());
    for (index, (expected_name, policy_input)) in
        expected_names.iter().zip(input.policies).enumerate()
    {
        if policy_input.name != *expected_name {
            return Err(format!("policy {index} is not {expected_name}"));
        }
        let valid_shape = match mode {
            CompileMode::PhaseOne => match index {
                0..=3 => {
                    policy_input.constraints.len() == 1 && policy_input.semantic_edge_count == 1
                }
                4 => policy_input.constraints.len() == 2 && policy_input.semantic_edge_count == 2,
                _ => false,
            },
            CompileMode::PhaseTwo => {
                let is_lane = index < 11;
                (is_lane
                    && policy_input.constraints.len() == 4
                    && policy_input.semantic_edge_count == 4)
                    || (!is_lane
                        && policy_input.semantic_edge_count == [20u16, 20u16, 12u16][index - 11])
            }
        };
        if !valid_shape {
            return Err(format!(
                "{} semantic expansion is incomplete",
                policy_input.name
            ));
        }
        let constraints = policy_input.constraints;
        let constraint_count = constraints.len();
        let specs = constraints
            .iter()
            .cloned()
            .into_iter()
            .map(semantic)
            .collect::<Result<Vec<_>, _>>()?;
        let seed = policy_seed_before
            .checked_add(1 + index as u64)
            .ok_or_else(|| "policy seed overflow".to_owned())?;
        let policy = derive_action_account(&settings, seed).0;
        let create = create_deployed_semantic_program_interaction_policy_instruction(
            settings,
            authority,
            delegated_signer,
            seed,
            input.account_index,
            specs.clone(),
        )
        .map_err(|error| error.to_string())?;
        let update = update_semantic_program_interaction_policy_instruction(
            settings,
            authority,
            policy,
            delegated_signer,
            input.account_index,
            specs,
        )
        .map_err(|error| error.to_string())?;
        let create_packet_bytes = packet_bytes(&create, authority)?;
        let update_packet_bytes = packet_bytes(&update, authority)?;
        if create_packet_bytes > PACKET_LIMIT || update_packet_bytes > PACKET_LIMIT {
            return Err(format!(
                "{} does not fit the first-safe packet rung: create={} update={}",
                policy_input.name, create_packet_bytes, update_packet_bytes
            ));
        }
        policies.push(PolicyOutput {
            name: policy_input.name,
            seed: seed.to_string(),
            policy: policy.to_string(),
            semantic_edge_count: policy_input.semantic_edge_count,
            constraint_count,
            constraints,
            create_packet_bytes,
            update_packet_bytes,
            create_instruction: wire(&create),
            update_instruction: wire(&update),
        });
    }
    Ok(Output {
        schema: "loyal-backyard-rwa-resolved-policy-artifact/v1",
        phase,
        verdict: "COMPILED_SIGNED_SIMULATION_REQUIRED",
        broadcast: false,
        physical_policy_count: policies.len(),
        policy_seed_before: policy_seed_before.to_string(),
        catalog_sha256: input.catalog_sha256,
        resolution_sha256: input.resolution_sha256,
        source_sha256,
        policies,
    })
}

fn main() -> Result<(), String> {
    let args = std::env::args().skip(1).collect::<Vec<_>>();
    let mode = match args.as_slice() {
        [] => CompileMode::PhaseTwo,
        [flag] if flag == "--phase1" => CompileMode::PhaseOne,
        _ => return Err("usage: compile-backyard-rwa-resolved-policies [--phase1]".to_owned()),
    };
    let mut source = Vec::new();
    std::io::stdin()
        .read_to_end(&mut source)
        .map_err(|error| error.to_string())?;
    let input: Input = serde_json::from_slice(&source)
        .map_err(|error| format!("invalid resolved policy input: {error}"))?;
    let output = compile(input, sha256(&source), mode)?;
    println!(
        "{}",
        serde_json::to_string(&output).map_err(|error| error.to_string())?
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn constraint(byte: u8) -> ConstraintInput {
        ConstraintInput {
            program_id: Pubkey::new_from_array([byte; 32]).to_string(),
            account_pubkeys: vec![AccountConstraintInput {
                index: 0,
                pubkeys: vec![Pubkey::new_from_array([byte.wrapping_add(1); 32]).to_string()],
            }],
            data: vec![DataConstraintInput::SliceEquals {
                offset: 0,
                value_hex: "0102030405060708".to_owned(),
            }],
        }
    }

    fn input(ready: bool) -> Input {
        Input {
            schema: "loyal-backyard-rwa-policy-compiler-input/v1".to_owned(),
            addresses_resolved: ready,
            swap_headers_resolved: ready,
            catalog_sha256: "11".repeat(32),
            resolution_sha256: "22".repeat(32),
            settings: SETTINGS.to_owned(),
            authority: AUTHORITY.to_owned(),
            delegated_signer: DELEGATED_SIGNER.to_owned(),
            account_index: 0,
            policy_seed_before: POLICY_SEED_BEFORE.to_string(),
            policies: EXPECTED_NAMES
                .iter()
                .enumerate()
                .map(|(index, name)| PolicyInput {
                    name: (*name).to_owned(),
                    semantic_edge_count: if index < 11 {
                        4
                    } else {
                        [20, 20, 12][index - 11]
                    },
                    constraints: if index < 11 {
                        (0..4)
                            .map(|offset| constraint((index * 4 + offset + 1) as u8))
                            .collect()
                    } else {
                        vec![constraint((index + 50) as u8)]
                    },
                })
                .collect(),
        }
    }

    #[test]
    fn compiles_exact_sequential_fourteen_policy_rung() {
        let output = compile(input(true), "33".repeat(32), CompileMode::PhaseTwo)
            .expect("resolved input compiles");
        assert_eq!(output.policies.len(), 14);
        assert_eq!(output.policies.first().unwrap().seed, "57");
        assert_eq!(output.policies.last().unwrap().seed, "70");
        assert!(output.policies.iter().all(|policy| {
            policy.create_packet_bytes <= PACKET_LIMIT && policy.update_packet_bytes <= PACKET_LIMIT
        }));
    }

    #[test]
    fn unresolved_graph_never_emits_policy_bytes() {
        let result = compile(input(false), "33".repeat(32), CompileMode::PhaseTwo);
        assert!(matches!(result, Err(error) if error.contains("readiness boundary")));
    }

    #[test]
    fn phase_one_compiles_only_four_exact_steps_and_two_swap_edges() {
        let mut value = input(true);
        value.policy_seed_before = "72".to_owned();
        value.policies = PHASE_ONE_NAMES
            .iter()
            .enumerate()
            .map(|(index, name)| PolicyInput {
                name: (*name).to_owned(),
                semantic_edge_count: if index < 4 { 1 } else { 2 },
                constraints: if index < 4 {
                    vec![constraint((index + 1) as u8)]
                } else {
                    vec![constraint(5), constraint(6)]
                },
            })
            .collect();
        let output = compile(value, "44".repeat(32), CompileMode::PhaseOne)
            .expect("phase one input compiles");
        assert_eq!(output.phase, "phase1");
        assert_eq!(output.policies.len(), 5);
        assert_eq!(output.policies[0].seed, "73");
        assert_eq!(output.policies[4].seed, "77");
    }
}
