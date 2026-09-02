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
use loyal_solana_env::solana_testing_keypair_from_env;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash,
    instruction::Instruction,
    message::Message,
    pubkey::Pubkey,
    signature::{Keypair, Signature},
    signer::Signer,
    transaction::Transaction,
};
use std::{io::Read, str::FromStr};

const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const DELEGATED_SIGNER: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
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
const PHASE_TWO_LANE_NAMES: [&str; 11] = [
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
];
const MIN_EXPIRED_MEASUREMENT_BLOCKHASH_GAP: u64 = 512;
const PHASE_ONE_NAMES: [&str; 5] = [
    "lane/Prime/PRIME/USDC/deposit",
    "lane/Prime/PRIME/USDC/borrow",
    "lane/Prime/PRIME/USDC/repay",
    "lane/Prime/PRIME/USDC/withdraw",
    "swap/Prime/PRIME/USDC",
];
const PHASE_ONE_FORWARD_ROLLOVER_NAMES: [&str; 1] = ["swap/Prime/USDC/PRIME/forward-rollover"];
const PHASE_ONE_FORWARD_ROLLOVER_SEED_BEFORE: u64 = 65;

#[derive(Clone, Copy, PartialEq, Eq)]
enum CompileMode {
    PhaseOne,
    PhaseOneForwardRollover,
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
    /// Confirmed slot and bytes hash from the Settings read that supplied
    /// `policy_seed_before`.  The compiler does not perform RPC itself, so a
    /// Phase-2 caller must carry that observation explicitly rather than rely
    /// on a historical seed baked into this binary.
    #[serde(default)]
    settings_context_slot: Option<u64>,
    #[serde(default)]
    settings_data_sha256: Option<String>,
    /// A deliberately old confirmed blockhash is used to produce authentic,
    /// non-broadcastable packet measurements.  It must predate the Settings
    /// observation by enough slots that it cannot execute if an evidence file
    /// escapes its intended review path.
    #[serde(default)]
    measurement_blockhash: Option<String>,
    #[serde(default)]
    measurement_blockhash_slot: Option<u64>,
    #[serde(default)]
    pending_swap: Option<PendingSwapInput>,
    policies: Vec<PolicyInput>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct PendingSwapInput {
    required_edge_count: u16,
    resolved_edge_count: u16,
    reason: String,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct PolicyInput {
    name: String,
    semantic_edge_count: u16,
    constraints: Vec<ConstraintInput>,
    #[serde(default)]
    swap_edges: Vec<SwapEdgeInput>,
}

/// One concrete directed edge carried by a resolved Jupiter constraint.  The
/// compiler verifies that every address is actually contained by the exact
/// constraint at the supplied account indexes; this keeps edge coverage from
/// degrading into an untrusted `semanticEdgeCount` label.
#[derive(Clone, Deserialize, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
struct SwapEdgeInput {
    from: String,
    to: String,
    constraint_index: usize,
    authority_index: u8,
    source_index: u8,
    destination_index: u8,
    source_mint_index: u8,
    destination_mint_index: u8,
    source_token_program_index: u8,
    destination_token_program_index: u8,
    authority: String,
    source_custody: String,
    destination_custody: String,
    source_mint: String,
    destination_mint: String,
    source_token_program: String,
    destination_token_program: String,
}

#[derive(Clone, Deserialize, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
struct ConstraintInput {
    #[serde(default)]
    operation: Option<String>,
    program_id: String,
    account_pubkeys: Vec<AccountConstraintInput>,
    data: Vec<DataConstraintInput>,
}

#[derive(Clone, Deserialize, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
struct AccountConstraintInput {
    index: u8,
    pubkeys: Vec<String>,
}

#[derive(Clone, Deserialize, PartialEq, Eq, Serialize)]
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
    #[serde(skip_serializing_if = "Option::is_none")]
    packing: Option<PackingOutput>,
    #[serde(skip_serializing_if = "Option::is_none")]
    swap: Option<SwapOutput>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    packet_measurements: Vec<PacketMeasurement>,
    policies: Vec<PolicyOutput>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct PackingOutput {
    selected_rung: String,
    attempted_rungs: Vec<PackingAttempt>,
    /// The first contiguous seeds are intentionally dependency-ordered so a
    /// capped sequential simulator can create and exercise representatives
    /// without first installing the entire catalog.
    activation_prefix: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    comparative_candidates: Vec<PackingCandidate>,
    #[serde(skip_serializing_if = "Option::is_none")]
    exact_swap_packing_proof: Option<ExactSwapPackingProof>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct PackingCandidate {
    name: String,
    physical_policy_count: usize,
    total_policy_create_data_bytes: usize,
    total_packet_bytes: usize,
    selected: bool,
}

/// Compact evidence for the exhaustive exact-edge arity boundary.  The raw
/// signed packets are intentionally not persisted 23,426 times; their packet
/// hashes are folded into the digest after each signature is verified.
#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ExactSwapPackingProof {
    pair_candidate_count: usize,
    pair_fit_count: usize,
    pair_min_packet_bytes: usize,
    pair_max_packet_bytes: usize,
    pair_policy_create_data_bytes: Vec<usize>,
    triple_candidate_count: usize,
    triple_fit_count: usize,
    triple_min_packet_bytes: usize,
    triple_max_packet_bytes: usize,
    all_signatures_verified: bool,
    measurement_set_sha256: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct PackingAttempt {
    rung: String,
    physical_policy_count: usize,
    fits: bool,
    measurements: Vec<PacketMeasurement>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct SwapOutput {
    status: &'static str,
    required_edge_count: u16,
    resolved_edge_count: u16,
    reason: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct PolicyOutput {
    name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    logical_name: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    operations: Vec<String>,
    seed: String,
    policy: String,
    semantic_edge_count: u16,
    constraint_count: usize,
    constraints: Vec<ConstraintInput>,
    swap_edges: Vec<SwapEdgeInput>,
    create_packet_bytes: usize,
    update_packet_bytes: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    create_packet: Option<PacketMeasurement>,
    create_instruction: WireInstruction,
    update_instruction: WireInstruction,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct PacketMeasurement {
    key: String,
    rung: String,
    policy_name: String,
    logical_name: String,
    seed: String,
    policy_create_data_bytes: usize,
    packet_bytes: usize,
    transaction_base64: String,
    transaction_sha256: String,
    message_sha256: String,
    signature: String,
    signature_verified: bool,
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

fn is_sha256(value: Option<&String>) -> bool {
    value.is_some_and(|value| {
        value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
    })
}

fn expected_swap_edges(name: &str) -> Result<std::collections::BTreeSet<String>, String> {
    let stable = ["USDC", "USDG", "USDS", "PYUSD"];
    let rwa = ["ONyc", "PRIME", "syrupUSDC", "AUTO", "USDe"];
    let edges = match name {
        "swap/stable-to-rwa" => stable
            .iter()
            .flat_map(|from| rwa.iter().map(move |to| format!("{from}->{to}")))
            .collect(),
        "swap/rwa-to-stable" => rwa
            .iter()
            .flat_map(|from| stable.iter().map(move |to| format!("{from}->{to}")))
            .collect(),
        "swap/stable-to-stable" => stable
            .iter()
            .flat_map(|from| {
                stable
                    .iter()
                    .filter(move |to| *to != from)
                    .map(move |to| format!("{from}->{to}"))
            })
            .collect(),
        _ => return Err(format!("{name} is not a recognized swap slice")),
    };
    Ok(edges)
}

fn constraint_contains(constraint: &ConstraintInput, index: u8, address: &str) -> bool {
    constraint
        .account_pubkeys
        .iter()
        .find(|candidate| candidate.index == index)
        .is_some_and(|candidate| {
            candidate
                .pubkeys
                .iter()
                .any(|candidate| candidate == address)
        })
}

fn validate_swap_slice(
    policy: &PolicyInput,
    expected_constraint_count: usize,
) -> Result<(), String> {
    let expected_edges = expected_swap_edges(&policy.name)?;
    if policy.semantic_edge_count as usize != expected_edges.len()
        || policy.constraints.len() != expected_constraint_count
        || policy.swap_edges.len() != expected_edges.len()
    {
        return Err(format!("{} semantic expansion is incomplete", policy.name));
    }
    let observed_edges = policy
        .swap_edges
        .iter()
        .map(|edge| format!("{}->{}", edge.from, edge.to))
        .collect::<std::collections::BTreeSet<_>>();
    if observed_edges != expected_edges {
        return Err(format!(
            "{} does not cover its exact directed edge set",
            policy.name
        ));
    }
    for edge in &policy.swap_edges {
        let constraint = policy
            .constraints
            .get(edge.constraint_index)
            .ok_or_else(|| {
                format!(
                    "{} edge {}->{} references a missing constraint",
                    policy.name, edge.from, edge.to
                )
            })?;
        for (index, address, label) in [
            (edge.authority_index, edge.authority.as_str(), "authority"),
            (
                edge.source_index,
                edge.source_custody.as_str(),
                "source custody",
            ),
            (
                edge.destination_index,
                edge.destination_custody.as_str(),
                "destination custody",
            ),
            (
                edge.source_mint_index,
                edge.source_mint.as_str(),
                "source mint",
            ),
            (
                edge.destination_mint_index,
                edge.destination_mint.as_str(),
                "destination mint",
            ),
            (
                edge.source_token_program_index,
                edge.source_token_program.as_str(),
                "source token program",
            ),
            (
                edge.destination_token_program_index,
                edge.destination_token_program.as_str(),
                "destination token program",
            ),
        ] {
            key(address, label)?;
            if !constraint_contains(constraint, index, address) {
                return Err(format!(
                    "{} edge {}->{} is not pinned to its {label}",
                    policy.name, edge.from, edge.to
                ));
            }
        }
    }
    Ok(())
}

#[derive(Clone)]
struct PhysicalPolicy {
    name: String,
    logical_name: String,
    operations: Vec<String>,
    constraints: Vec<ConstraintInput>,
    swap_edges: Vec<SwapEdgeInput>,
}

fn has_swap_edge(policy: &PhysicalPolicy, from: &str, to: &str) -> bool {
    policy
        .swap_edges
        .iter()
        .any(|edge| edge.from == from && edge.to == to)
}

fn take_activation_policy(
    remaining: &mut Vec<PhysicalPolicy>,
    label: &str,
    predicate: impl Fn(&PhysicalPolicy) -> bool,
) -> Result<PhysicalPolicy, String> {
    let index = remaining
        .iter()
        .position(predicate)
        .ok_or_else(|| format!("Phase-2 activation prefix is missing {label}"))?;
    Ok(remaining.remove(index))
}

/// A ten-policy prefix for the provider's 20-transaction sequential cap.
/// Swaps are deliberately created before the collateral deposits they fund;
/// the two remaining swap families are then available as independently
/// testable representatives.  All omitted policies retain their deterministic
/// compiler order immediately after this prefix.
fn activation_prefix_order(
    selected: Vec<PhysicalPolicy>,
) -> Result<(Vec<PhysicalPolicy>, Vec<String>), String> {
    let mut remaining = selected;
    let mut prefix = Vec::with_capacity(10);
    let mut take =
        |label: &str, predicate: &dyn Fn(&PhysicalPolicy) -> bool| -> Result<(), String> {
            prefix.push(take_activation_policy(&mut remaining, label, predicate)?);
            Ok(())
        };
    take("USDC->ONyc stable-to-RWA swap", &|policy| {
        has_swap_edge(policy, "USDC", "ONyc")
    })?;
    take("OnRe deposit", &|policy| {
        policy.name == "lane/OnRe/ONyc/USDC/deposit"
    })?;
    take("Prime deposit", &|policy| {
        policy.name == "lane/Prime/PRIME/USDC/deposit"
    })?;
    take("USDC->syrupUSDC stable-to-RWA swap", &|policy| {
        has_swap_edge(policy, "USDC", "syrupUSDC")
    })?;
    take("Maple deposit", &|policy| {
        policy.name == "lane/Maple/syrupUSDC/USDC/deposit"
    })?;
    take("AUTO deposit", &|policy| {
        policy.name == "lane/AUTO/AUTO/PYUSD/deposit"
    })?;
    take("USDC->USDe stable-to-RWA swap", &|policy| {
        has_swap_edge(policy, "USDC", "USDe")
    })?;
    take("Ethena deposit", &|policy| {
        policy.name == "lane/Ethena/USDe/PYUSD/deposit"
    })?;
    take("PRIME->USDC RWA-to-stable swap", &|policy| {
        has_swap_edge(policy, "PRIME", "USDC")
    })?;
    take("USDC->USDG stable-to-stable swap", &|policy| {
        has_swap_edge(policy, "USDC", "USDG")
    })?;
    let prefix_names = prefix.iter().map(|policy| policy.name.clone()).collect();
    prefix.extend(remaining);
    Ok((prefix, prefix_names))
}

fn phase_two_operation_name(constraint: &ConstraintInput) -> Result<String, String> {
    let operation = constraint
        .operation
        .as_deref()
        .ok_or_else(|| "Phase-2 Kamino constraint is missing its operation name".to_owned())?;
    if !matches!(operation, "deposit" | "borrow" | "repay" | "withdraw") {
        return Err(format!(
            "{operation} is not an exact Phase-2 Kamino operation"
        ));
    }
    Ok(operation.to_owned())
}

fn phase_two_lane_logical_policies(input: &[PolicyInput]) -> Result<Vec<PhysicalPolicy>, String> {
    if input.len() != PHASE_TWO_LANE_NAMES.len() {
        return Err(
            "Phase-2 Kamino compiler requires exactly the eleven resolved lanes".to_owned(),
        );
    }
    input
        .iter()
        .zip(PHASE_TWO_LANE_NAMES)
        .map(|(policy, expected_name)| {
            if policy.name != expected_name
                || policy.semantic_edge_count != 4
                || policy.constraints.len() != 4
                || !policy.swap_edges.is_empty()
            {
                return Err(format!(
                    "{} is not an exact four-operation lane",
                    policy.name
                ));
            }
            let operations = policy
                .constraints
                .iter()
                .map(phase_two_operation_name)
                .collect::<Result<Vec<_>, _>>()?;
            if operations != ["deposit", "borrow", "repay", "withdraw"] {
                return Err(format!(
                    "{} operations must be deposit, borrow, repay, withdraw in exact SDK order",
                    policy.name
                ));
            }
            Ok(PhysicalPolicy {
                name: policy.name.clone(),
                logical_name: policy.name.clone(),
                operations,
                constraints: policy.constraints.clone(),
                swap_edges: vec![],
            })
        })
        .collect()
}

/// Every contiguous operation partition for a four-operation Kamino lane.
///
/// The ABI can combine *complete* instruction constraints in one policy, but has no
/// shared outer account/data allowlist.  These partitions therefore retain every
/// operation as an independent exact alternative; they never turn e.g. a deposit
/// account list and a borrow account list into a cross-product.
fn kamino_partitions(policy: &PhysicalPolicy) -> Vec<(String, Vec<PhysicalPolicy>)> {
    debug_assert_eq!(policy.constraints.len(), 4);
    (0_u8..8)
        .map(|cuts| {
            let mut start = 0_usize;
            let mut groups = Vec::new();
            for index in 0..4 {
                let end = index + 1;
                if index == 3 || (cuts & (1 << index)) != 0 {
                    let operations = policy.operations[start..end].to_vec();
                    groups.push(PhysicalPolicy {
                        name: format!("{}/{}", policy.logical_name, operations.join("-")),
                        logical_name: policy.logical_name.clone(),
                        operations,
                        constraints: policy.constraints[start..end].to_vec(),
                        swap_edges: vec![],
                    });
                    start = end;
                }
            }
            let label = groups
                .iter()
                .map(|group| group.operations.len().to_string())
                .collect::<Vec<_>>()
                .join("+");
            (label, groups)
        })
        .collect()
}

fn phase_two_rungs(logical: &[PhysicalPolicy]) -> Vec<(String, Vec<PhysicalPolicy>)> {
    // Keep every coarse partition in the evidence: a rejected 1+3 partition is
    // materially different from a rejected 2+2 partition when packet layout moves.
    let labels = [
        "4", "1+3", "2+2", "1+1+2", "3+1", "1+2+1", "2+1+1", "1+1+1+1",
    ];
    labels
        .iter()
        .map(|label| {
            let policies = logical
                .iter()
                .flat_map(|policy| {
                    kamino_partitions(policy)
                        .into_iter()
                        .find_map(|(candidate_label, groups)| {
                            (candidate_label == *label).then_some(groups)
                        })
                        .expect("all fixed four-operation partitions exist")
                })
                .collect();
            (format!("partition-{label}"), policies)
        })
        .collect()
}

fn validate_swap_edge_against_constraints(
    policy_name: &str,
    constraints: &[ConstraintInput],
    edge: &SwapEdgeInput,
) -> Result<(), String> {
    let constraint = constraints
        .get(edge.constraint_index)
        .ok_or_else(|| format!("{policy_name} edge references a missing constraint"))?;
    for (index, address, label) in [
        (edge.authority_index, edge.authority.as_str(), "authority"),
        (
            edge.source_index,
            edge.source_custody.as_str(),
            "source custody",
        ),
        (
            edge.destination_index,
            edge.destination_custody.as_str(),
            "destination custody",
        ),
        (
            edge.source_mint_index,
            edge.source_mint.as_str(),
            "source mint",
        ),
        (
            edge.destination_mint_index,
            edge.destination_mint.as_str(),
            "destination mint",
        ),
        (
            edge.source_token_program_index,
            edge.source_token_program.as_str(),
            "source token program",
        ),
        (
            edge.destination_token_program_index,
            edge.destination_token_program.as_str(),
            "destination token program",
        ),
    ] {
        key(address, label)?;
        if !constraint_contains(constraint, index, address) {
            return Err(format!(
                "{} edge {}->{} is not pinned to its {label}",
                policy_name, edge.from, edge.to
            ));
        }
    }
    Ok(())
}

fn validate_swap_edge(policy: &PolicyInput, edge: &SwapEdgeInput) -> Result<(), String> {
    validate_swap_edge_against_constraints(&policy.name, &policy.constraints, edge)
}

fn phase_two_swap_logical_policies(input: &[PolicyInput]) -> Result<Vec<PhysicalPolicy>, String> {
    let slice_names = [
        "swap/stable-to-rwa",
        "swap/rwa-to-stable",
        "swap/stable-to-stable",
    ];
    if input.len() != 52 {
        return Err(
            "Phase-2 swap compiler requires exactly 52 per-edge logical policies".to_owned(),
        );
    }
    let mut observed = std::collections::BTreeSet::new();
    let mut output = Vec::with_capacity(input.len());
    for policy in input {
        if policy.semantic_edge_count != 1
            || policy.constraints.len() != 1
            || policy.swap_edges.len() != 1
            || policy.constraints[0].operation.is_some()
        {
            return Err(format!(
                "{} is not an exact one-edge Jupiter policy",
                policy.name
            ));
        }
        let edge = &policy.swap_edges[0];
        let slice = match (edge.from.as_str(), edge.to.as_str()) {
            (
                "USDC" | "USDG" | "USDS" | "PYUSD",
                "ONyc" | "PRIME" | "syrupUSDC" | "AUTO" | "USDe",
            ) => slice_names[0],
            (
                "ONyc" | "PRIME" | "syrupUSDC" | "AUTO" | "USDe",
                "USDC" | "USDG" | "USDS" | "PYUSD",
            ) => slice_names[1],
            ("USDC" | "USDG" | "USDS" | "PYUSD", "USDC" | "USDG" | "USDS" | "PYUSD")
                if edge.from != edge.to =>
            {
                slice_names[2]
            }
            _ => {
                return Err(format!(
                    "{} has an unsupported Jupiter edge {}->{}",
                    policy.name, edge.from, edge.to
                ))
            }
        };
        if policy.name != format!("{slice}/{}->{}", edge.from, edge.to) {
            return Err(format!(
                "{} does not name its exact Jupiter edge",
                policy.name
            ));
        }
        validate_swap_edge(policy, edge)?;
        if !observed.insert(format!("{}->{}", edge.from, edge.to)) {
            return Err("Phase-2 Jupiter edge catalog contains a duplicate".to_owned());
        }
        output.push(PhysicalPolicy {
            name: policy.name.clone(),
            logical_name: policy.name.clone(),
            operations: vec![],
            constraints: policy.constraints.clone(),
            swap_edges: policy.swap_edges.clone(),
        });
    }
    let expected = slice_names
        .iter()
        .flat_map(|name| expected_swap_edges(name).expect("fixed swap slice"))
        .collect::<std::collections::BTreeSet<_>>();
    if observed != expected {
        return Err(
            "Phase-2 Jupiter edge catalog does not have the exact 52-edge bijection".to_owned(),
        );
    }
    Ok(output)
}

fn phase_two_swap_rungs(logical: &[PhysicalPolicy]) -> Vec<(&'static str, Vec<PhysicalPolicy>)> {
    let slices = [
        "swap/stable-to-rwa",
        "swap/rwa-to-stable",
        "swap/stable-to-stable",
    ]
    .iter()
    .map(|slice| {
        let members = logical
            .iter()
            .filter(|policy| policy.logical_name.starts_with(&format!("{slice}/")))
            .collect::<Vec<_>>();
        let mut merged = merge_swap_bin(0, &members.into_iter().cloned().collect::<Vec<_>>());
        merged.name = (*slice).to_owned();
        merged.logical_name = (*slice).to_owned();
        merged
    })
    .collect::<Vec<_>>();
    let source = logical
        .iter()
        .fold(
            std::collections::BTreeMap::<String, Vec<&PhysicalPolicy>>::new(),
            |mut groups, policy| {
                groups
                    .entry(policy.swap_edges[0].from.clone())
                    .or_default()
                    .push(policy);
                groups
            },
        )
        .into_iter()
        .map(|(from, members)| {
            let mut merged = merge_swap_bin(0, &members.into_iter().cloned().collect::<Vec<_>>());
            merged.name = format!("swap/source/{from}");
            merged.logical_name = format!("swap/source/{from}");
            merged
        })
        .collect::<Vec<_>>();
    vec![
        ("swap-slice", slices),
        ("swap-source", source),
        ("swap-edge", logical.to_vec()),
    ]
}

fn signed_create_packet(
    instruction: &Instruction,
    authority: &Keypair,
    measurement_blockhash: Hash,
    rung: &str,
    policy: &PhysicalPolicy,
    seed: u64,
) -> Result<PacketMeasurement, String> {
    let transaction = Transaction::new_signed_with_payer(
        std::slice::from_ref(instruction),
        Some(&authority.pubkey()),
        &[authority],
        measurement_blockhash,
    );
    transaction
        .verify()
        .map_err(|error| format!("signed PolicyCreate packet does not verify: {error}"))?;
    let wire = bincode::serialize(&transaction).map_err(|error| error.to_string())?;
    let message = transaction.message.serialize();
    Ok(PacketMeasurement {
        key: format!("{}/{}/create", rung, policy.name),
        rung: rung.to_owned(),
        policy_name: policy.name.clone(),
        logical_name: policy.logical_name.clone(),
        seed: seed.to_string(),
        policy_create_data_bytes: instruction.data.len(),
        packet_bytes: wire.len(),
        transaction_base64: BASE64.encode(&wire),
        transaction_sha256: sha256(&wire),
        message_sha256: sha256(&message),
        signature: transaction
            .signatures
            .first()
            .ok_or_else(|| "signed PolicyCreate is missing its authority signature".to_owned())?
            .to_string(),
        signature_verified: true,
    })
}

fn measurement_signer(authority: Pubkey) -> Result<Keypair, String> {
    let signer = solana_testing_keypair_from_env().map_err(|_| {
        "SOLANA_TESTING_PK is required for actual signed Phase-2 PolicyCreate measurements"
            .to_owned()
    })?;
    if signer.pubkey() != authority {
        return Err(
            "SOLANA_TESTING_PK does not match the pinned Squads Settings authority".to_owned(),
        );
    }
    Ok(signer)
}

fn measure_rung(
    rung: &str,
    candidates: &[PhysicalPolicy],
    policy_seed_before: u64,
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    signer: &Keypair,
    measurement_blockhash: Hash,
) -> Result<PackingAttempt, String> {
    let measurements = candidates
        .iter()
        .enumerate()
        .map(|(index, candidate)| {
            let seed = policy_seed_before
                .checked_add(index as u64 + 1)
                .ok_or_else(|| "policy seed overflow".to_owned())?;
            let specs = candidate
                .constraints
                .iter()
                .cloned()
                .map(semantic)
                .collect::<Result<Vec<_>, _>>()?;
            let create = create_deployed_semantic_program_interaction_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seed,
                account_index,
                specs,
            )
            .map_err(|error| error.to_string())?;
            signed_create_packet(
                &create,
                signer,
                measurement_blockhash,
                rung,
                candidate,
                seed,
            )
        })
        .collect::<Result<Vec<_>, String>>()?;
    let fits = measurements
        .iter()
        .all(|measurement| measurement.packet_bytes <= PACKET_LIMIT);
    Ok(PackingAttempt {
        rung: rung.to_owned(),
        physical_policy_count: candidates.len(),
        fits,
        measurements,
    })
}

fn packing_objective(measurements: &[PacketMeasurement]) -> (usize, usize, usize) {
    (
        measurements
            .iter()
            .map(|measurement| measurement.policy_create_data_bytes)
            .sum(),
        measurements.len(),
        measurements
            .iter()
            .map(|measurement| measurement.packet_bytes)
            .sum(),
    )
}

fn packing_candidate(
    name: String,
    measurements: &[PacketMeasurement],
    selected: bool,
) -> PackingCandidate {
    PackingCandidate {
        name,
        physical_policy_count: measurements.len(),
        total_policy_create_data_bytes: measurements
            .iter()
            .map(|measurement| measurement.policy_create_data_bytes)
            .sum(),
        total_packet_bytes: measurements
            .iter()
            .map(|measurement| measurement.packet_bytes)
            .sum(),
        selected,
    }
}

fn merge_swap_bin(index: usize, members: &[PhysicalPolicy]) -> PhysicalPolicy {
    let mut constraint_offset = 0_usize;
    let mut constraints = Vec::new();
    let mut swap_edges = Vec::new();
    for member in members {
        constraints.extend(member.constraints.iter().cloned());
        swap_edges.extend(member.swap_edges.iter().cloned().map(|mut edge| {
            edge.constraint_index += constraint_offset;
            edge
        }));
        constraint_offset += member.constraints.len();
    }
    PhysicalPolicy {
        name: format!("swap/packed/{:02}", index + 1),
        logical_name: format!("swap/packed/{:02}", index + 1),
        operations: vec![],
        constraints,
        swap_edges,
    }
}

fn validate_packed_swap_policy(policy: &PhysicalPolicy) -> Result<(), String> {
    if policy.constraints.len() != policy.swap_edges.len() {
        return Err(format!(
            "{} packed swap has {} constraints but {} edges",
            policy.name,
            policy.constraints.len(),
            policy.swap_edges.len()
        ));
    }
    for edge in &policy.swap_edges {
        validate_swap_edge_against_constraints(&policy.name, &policy.constraints, edge)?;
    }
    Ok(())
}

fn pack_swap_best_fit(
    logical: &[PhysicalPolicy],
    order: &[usize],
    policy_seed_before: u64,
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    signer: &Keypair,
    measurement_blockhash: Hash,
) -> Result<Vec<PhysicalPolicy>, String> {
    let mut bins: Vec<Vec<PhysicalPolicy>> = Vec::new();
    for &entry in order {
        let member = logical
            .get(entry)
            .ok_or_else(|| "swap packing order drifted".to_owned())?
            .clone();
        let mut best: Option<(usize, usize)> = None;
        for index in 0..bins.len() {
            let mut candidate_members = bins[index].clone();
            candidate_members.push(member.clone());
            let candidate = merge_swap_bin(index, &candidate_members);
            validate_packed_swap_policy(&candidate)?;
            let attempt = measure_rung(
                "swap/byte-optimal-probe",
                std::slice::from_ref(&candidate),
                policy_seed_before + index as u64,
                settings,
                authority,
                delegated_signer,
                account_index,
                signer,
                measurement_blockhash,
            )?;
            let bytes = attempt.measurements[0].packet_bytes;
            if bytes <= PACKET_LIMIT && best.is_none_or(|(_, current)| bytes > current) {
                best = Some((index, bytes));
            }
        }
        if let Some((index, _)) = best {
            bins[index].push(member);
        } else {
            bins.push(vec![member]);
        }
    }
    let packed = bins
        .iter()
        .enumerate()
        .map(|(index, members)| merge_swap_bin(index, members))
        .collect::<Vec<_>>();
    for policy in &packed {
        validate_packed_swap_policy(policy)?;
    }
    Ok(packed)
}

fn swap_packing_orders(
    logical: &[PhysicalPolicy],
    baseline: &[PacketMeasurement],
) -> Vec<(String, Vec<usize>)> {
    let sizes = baseline
        .iter()
        .map(|measurement| (measurement.logical_name.as_str(), measurement.packet_bytes))
        .collect::<std::collections::BTreeMap<_, _>>();
    let size = |index: &usize| {
        *sizes
            .get(logical[*index].logical_name.as_str())
            .expect("baseline exact edge")
    };
    let mut by_size = (0..logical.len()).collect::<Vec<_>>();
    by_size.sort_by_key(|index| std::cmp::Reverse(size(index)));
    let mut by_source = by_size.clone();
    by_source.sort_by_key(|index| {
        let edge = &logical[*index].swap_edges[0];
        (edge.from.clone(), std::cmp::Reverse(size(index)))
    });
    let mut by_destination = by_size.clone();
    by_destination.sort_by_key(|index| {
        let edge = &logical[*index].swap_edges[0];
        (edge.to.clone(), std::cmp::Reverse(size(index)))
    });
    let mut ascending = by_size.clone();
    ascending.reverse();
    vec![
        ("swap/byte-optimal-best-fit-size".to_owned(), by_size),
        ("swap/byte-optimal-best-fit-source".to_owned(), by_source),
        (
            "swap/byte-optimal-best-fit-destination".to_owned(),
            by_destination,
        ),
        ("swap/byte-optimal-best-fit-ascending".to_owned(), ascending),
    ]
}

fn exhaustive_exact_swap_packing_proof(
    logical: &[PhysicalPolicy],
    policy_seed_before: u64,
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    signer: &Keypair,
    measurement_blockhash: Hash,
) -> Result<ExactSwapPackingProof, String> {
    let mut pair_candidate_count = 0;
    let mut pair_fit_count = 0;
    let mut pair_min_packet_bytes = usize::MAX;
    let mut pair_max_packet_bytes = 0;
    let mut pair_data_bytes = std::collections::BTreeSet::new();
    let mut triple_candidate_count = 0;
    let mut triple_fit_count = 0;
    let mut triple_min_packet_bytes = usize::MAX;
    let mut triple_max_packet_bytes = 0;
    let mut digest = Sha256::new();

    let mut observe = |arity: usize, members: Vec<PhysicalPolicy>| -> Result<(), String> {
        let candidate = merge_swap_bin(0, &members);
        let attempt = measure_rung(
            if arity == 2 {
                "swap/exhaustive-exact-pairs"
            } else {
                "swap/exhaustive-exact-triples"
            },
            std::slice::from_ref(&candidate),
            policy_seed_before,
            settings,
            authority,
            delegated_signer,
            account_index,
            signer,
            measurement_blockhash,
        )?;
        let measurement = &attempt.measurements[0];
        if !measurement.signature_verified {
            return Err("exhaustive swap measurement has an unverified signature".to_owned());
        }
        digest.update(measurement.transaction_sha256.as_bytes());
        if arity == 2 {
            pair_candidate_count += 1;
            pair_fit_count += usize::from(attempt.fits);
            pair_min_packet_bytes = pair_min_packet_bytes.min(measurement.packet_bytes);
            pair_max_packet_bytes = pair_max_packet_bytes.max(measurement.packet_bytes);
            pair_data_bytes.insert(measurement.policy_create_data_bytes);
        } else {
            triple_candidate_count += 1;
            triple_fit_count += usize::from(attempt.fits);
            triple_min_packet_bytes = triple_min_packet_bytes.min(measurement.packet_bytes);
            triple_max_packet_bytes = triple_max_packet_bytes.max(measurement.packet_bytes);
        }
        Ok(())
    };

    for first in 0..logical.len() {
        for second in (first + 1)..logical.len() {
            observe(2, vec![logical[first].clone(), logical[second].clone()])?;
            for third in (second + 1)..logical.len() {
                observe(
                    3,
                    vec![
                        logical[first].clone(),
                        logical[second].clone(),
                        logical[third].clone(),
                    ],
                )?;
            }
        }
    }
    Ok(ExactSwapPackingProof {
        pair_candidate_count,
        pair_fit_count,
        pair_min_packet_bytes,
        pair_max_packet_bytes,
        pair_policy_create_data_bytes: pair_data_bytes.into_iter().collect(),
        triple_candidate_count,
        triple_fit_count,
        triple_min_packet_bytes,
        triple_max_packet_bytes,
        all_signatures_verified: true,
        measurement_set_sha256: format!("{:x}", digest.finalize()),
    })
}

fn compile_phase_two(
    input: Input,
    source_sha256: String,
    policy_seed_before: u64,
) -> Result<Output, String> {
    let settings = key(&input.settings, "settings")?;
    let authority = key(&input.authority, "authority")?;
    let delegated_signer = key(&input.delegated_signer, "delegated signer")?;
    let settings_context_slot = input
        .settings_context_slot
        .ok_or_else(|| "Phase-2 requires the confirmed Settings observation slot".to_owned())?;
    let full_swap = input.swap_headers_resolved;
    if !input.addresses_resolved
        || !is_sha256(input.settings_data_sha256.as_ref())
        || settings_context_slot == 0
    {
        return Err("Phase-2 Kamino readiness boundary is incomplete".to_owned());
    }
    if full_swap {
        if input.pending_swap.is_some() || input.policies.len() != 63 {
            return Err(
                "resolved Phase-2 swap input must contain the eleven lanes plus 52 exact edges"
                    .to_owned(),
            );
        }
    } else if input.pending_swap.as_ref().is_none_or(|pending| {
        pending.required_edge_count != 52
            || pending.resolved_edge_count >= pending.required_edge_count
            || pending.reason.is_empty()
    }) || input.policies.len() != 11
    {
        return Err("Phase-2 requires an explicit pending-Jupiter boundary".to_owned());
    }
    let measurement_slot = input.measurement_blockhash_slot.ok_or_else(|| {
        "Phase-2 requires an expired confirmed measurement blockhash slot".to_owned()
    })?;
    if measurement_slot.saturating_add(MIN_EXPIRED_MEASUREMENT_BLOCKHASH_GAP)
        > settings_context_slot
    {
        return Err("measurement blockhash is too recent; use a confirmed blockhash at least 512 slots before Settings".to_owned());
    }
    let measurement_blockhash =
        Hash::from_str(input.measurement_blockhash.as_deref().ok_or_else(|| {
            "Phase-2 requires an expired confirmed measurement blockhash".to_owned()
        })?)
        .map_err(|_| "measurement blockhash is invalid".to_owned())?;
    let logical = phase_two_lane_logical_policies(&input.policies[..11])?;
    let signer = measurement_signer(authority)?;
    let mut attempts = Vec::new();
    let mut selected: Option<(String, Vec<PhysicalPolicy>, Vec<PacketMeasurement>)> = None;
    for (rung, candidates) in phase_two_rungs(&logical) {
        let attempt = measure_rung(
            &format!("kamino/{rung}"),
            &candidates,
            policy_seed_before,
            settings,
            authority,
            delegated_signer,
            input.account_index,
            &signer,
            measurement_blockhash,
        )?;
        if attempt.fits
            && selected.as_ref().is_none_or(|(_, _, current)| {
                packing_objective(&attempt.measurements) < packing_objective(current)
            })
        {
            selected = Some((rung, candidates, attempt.measurements.clone()));
        }
        attempts.push(attempt);
    }
    let (kamino_rung, mut selected_policies, _) = selected
        .ok_or_else(|| "no complete Phase-2 Kamino packet-fitting rung exists".to_owned())?;
    let (swap_status, selected_swap_rung, comparative_candidates, exact_swap_packing_proof) =
        if full_swap {
            let logical_swaps = phase_two_swap_logical_policies(&input.policies[11..])?;
            let swap_seed_before = policy_seed_before
                .checked_add(selected_policies.len() as u64)
                .ok_or_else(|| "policy seed overflow".to_owned())?;
            let mut baseline: Option<PackingAttempt> = None;
            for (rung, candidates) in phase_two_swap_rungs(&logical_swaps) {
                let attempt = measure_rung(
                    &format!("swap/{rung}"),
                    &candidates,
                    swap_seed_before,
                    settings,
                    authority,
                    delegated_signer,
                    input.account_index,
                    &signer,
                    measurement_blockhash,
                )?;
                if rung == "swap-edge" {
                    baseline = Some(attempt.clone());
                }
                attempts.push(attempt);
            }
            let baseline = baseline
                .filter(|attempt| attempt.fits)
                .ok_or_else(|| "the exact one-edge Jupiter baseline does not fit".to_owned())?;
            let exact_proof = exhaustive_exact_swap_packing_proof(
                &logical_swaps,
                swap_seed_before,
                settings,
                authority,
                delegated_signer,
                input.account_index,
                &signer,
                measurement_blockhash,
            )?;
            if exact_proof.pair_candidate_count != 1_326
                || exact_proof.pair_fit_count != exact_proof.pair_candidate_count
                || exact_proof.triple_candidate_count != 22_100
                || exact_proof.triple_fit_count != 0
            {
                return Err("exact Jupiter packing arity boundary is not proven".to_owned());
            }
            let mut best = (
                "swap-edge-baseline".to_owned(),
                logical_swaps.clone(),
                baseline.measurements.clone(),
            );
            let mut candidates = vec![packing_candidate(best.0.clone(), &best.2, false)];
            for (name, order) in swap_packing_orders(&logical_swaps, &baseline.measurements) {
                let policies = pack_swap_best_fit(
                    &logical_swaps,
                    &order,
                    swap_seed_before,
                    settings,
                    authority,
                    delegated_signer,
                    input.account_index,
                    &signer,
                    measurement_blockhash,
                )?;
                let attempt = measure_rung(
                    &name,
                    &policies,
                    swap_seed_before,
                    settings,
                    authority,
                    delegated_signer,
                    input.account_index,
                    &signer,
                    measurement_blockhash,
                )?;
                if !attempt.fits {
                    return Err(format!("{name} selected an oversized swap packet"));
                }
                candidates.push(packing_candidate(
                    name.clone(),
                    &attempt.measurements,
                    false,
                ));
                if packing_objective(&attempt.measurements) < packing_objective(&best.2) {
                    best = (name, policies, attempt.measurements.clone());
                }
                attempts.push(attempt);
            }
            if best.0 != "swap-edge-baseline" {
                let selected = candidates
                    .iter_mut()
                    .find(|candidate| candidate.name == best.0)
                    .expect("selected byte-optimal candidate recorded");
                selected.selected = true;
            } else {
                candidates[0].selected = true;
            }
            selected_policies.extend(best.1);
            (
                SwapOutput {
                    status: "COMPILED_52_EDGES",
                    required_edge_count: 52,
                    resolved_edge_count: 52,
                    reason: format!("selected exact byte-fit Jupiter packing {}", best.0),
                },
                best.0,
                candidates,
                Some(exact_proof),
            )
        } else {
            let pending = input.pending_swap.as_ref().expect("validated pending swap");
            (
                SwapOutput {
                    status: "PENDING_JUPITER_HEADERS",
                    required_edge_count: pending.required_edge_count,
                    resolved_edge_count: pending.resolved_edge_count,
                    reason: pending.reason.clone(),
                },
                "swap-pending".to_owned(),
                vec![],
                None,
            )
        };
    let selected_rung = if full_swap {
        format!("kamino:{kamino_rung}+{selected_swap_rung}")
    } else {
        format!("kamino:{kamino_rung}")
    };
    let (selected_policies, activation_prefix) = if full_swap {
        activation_prefix_order(selected_policies)?
    } else {
        (selected_policies, vec![])
    };
    for policy in selected_policies
        .iter()
        .filter(|policy| !policy.swap_edges.is_empty())
    {
        validate_packed_swap_policy(policy)?;
    }
    let selected_attempt = measure_rung(
        "selected/activation-prefix",
        &selected_policies,
        policy_seed_before,
        settings,
        authority,
        delegated_signer,
        input.account_index,
        &signer,
        measurement_blockhash,
    )?;
    if !selected_attempt.fits {
        return Err("activation-prefix order produced an oversized signed packet".to_owned());
    }
    let selected_packets = selected_attempt.measurements.clone();
    attempts.push(selected_attempt);
    let policies = selected_policies
        .into_iter()
        .zip(selected_packets.iter().cloned())
        .map(|(candidate, measurement)| {
            let seed = measurement
                .seed
                .parse::<u64>()
                .map_err(|_| "measurement seed drifted".to_owned())?;
            let specs = candidate
                .constraints
                .iter()
                .cloned()
                .map(semantic)
                .collect::<Result<Vec<_>, _>>()?;
            let create = create_deployed_semantic_program_interaction_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seed,
                input.account_index,
                specs,
            )
            .map_err(|error| error.to_string())?;
            Ok(PolicyOutput {
                name: candidate.name,
                logical_name: Some(candidate.logical_name),
                operations: candidate.operations,
                seed: seed.to_string(),
                policy: derive_action_account(&settings, seed).0.to_string(),
                semantic_edge_count: candidate.constraints.len() as u16,
                constraint_count: candidate.constraints.len(),
                constraints: candidate.constraints,
                swap_edges: candidate.swap_edges,
                create_packet_bytes: measurement.packet_bytes,
                update_packet_bytes: 0,
                create_packet: Some(measurement),
                create_instruction: wire(&create),
                update_instruction: WireInstruction {
                    program_id: String::new(),
                    accounts: vec![],
                    data_base64: String::new(),
                    data_sha256: String::new(),
                },
            })
        })
        .collect::<Result<Vec<_>, String>>()?;
    let packet_measurements = attempts
        .iter()
        .flat_map(|attempt| attempt.measurements.iter().cloned())
        .collect();
    Ok(Output {
        schema: "loyal-backyard-rwa-resolved-policy-artifact/v1",
        phase: if full_swap { "phase2" } else { "phase2-kamino" },
        verdict: if full_swap {
            "COMPILED_SIGNED_SIMULATION_REQUIRED"
        } else {
            "KAMINO_COMPILED_SWAP_PENDING"
        },
        broadcast: false,
        physical_policy_count: policies.len(),
        policy_seed_before: policy_seed_before.to_string(),
        catalog_sha256: input.catalog_sha256,
        resolution_sha256: input.resolution_sha256,
        source_sha256,
        packing: Some(PackingOutput {
            selected_rung,
            attempted_rungs: attempts,
            activation_prefix,
            comparative_candidates,
            exact_swap_packing_proof,
        }),
        swap: Some(swap_status),
        packet_measurements,
        policies,
    })
}

fn compile(input: Input, source_sha256: String, mode: CompileMode) -> Result<Output, String> {
    let policy_seed_before = input
        .policy_seed_before
        .parse::<u64>()
        .map_err(|_| "policySeedBefore is not a u64".to_owned())?;
    if mode == CompileMode::PhaseTwo {
        if input.schema != "loyal-backyard-rwa-policy-compiler-input/v1"
            || input.catalog_sha256.len() != 64
            || input.resolution_sha256.len() != 64
            || input.settings != SETTINGS
            || input.authority != AUTHORITY
            || input.delegated_signer != DELEGATED_SIGNER
            || input.account_index != 0
        {
            return Err("resolved compiler identity boundary is incomplete or drifted".to_owned());
        }
        return compile_phase_two(input, source_sha256, policy_seed_before);
    }
    let (phase, expected_names): (&str, &[&str]) = match mode {
        CompileMode::PhaseOne => ("phase1", &PHASE_ONE_NAMES),
        CompileMode::PhaseOneForwardRollover => (
            "phase1-forward-jupiter-rollover",
            &PHASE_ONE_FORWARD_ROLLOVER_NAMES,
        ),
        CompileMode::PhaseTwo => unreachable!("handled above"),
    };
    if input.schema != "loyal-backyard-rwa-policy-compiler-input/v1"
        || !input.addresses_resolved
        || !input.swap_headers_resolved
        || input.catalog_sha256.len() != 64
        || input.resolution_sha256.len() != 64
        || input.settings != SETTINGS
        || input.authority != AUTHORITY
        || input.delegated_signer != DELEGATED_SIGNER
        || input.account_index != 0
        || (mode == CompileMode::PhaseTwo
            && (input.settings_context_slot.unwrap_or_default() == 0
                || !is_sha256(input.settings_data_sha256.as_ref())))
        || (mode == CompileMode::PhaseOneForwardRollover
            && policy_seed_before != PHASE_ONE_FORWARD_ROLLOVER_SEED_BEFORE)
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
            CompileMode::PhaseOneForwardRollover => {
                index == 0
                    && policy_input.constraints.len() == 2
                    && policy_input.semantic_edge_count == 2
            }
            CompileMode::PhaseTwo => {
                let is_lane = index < 11;
                (is_lane
                    && policy_input.constraints.len() == 4
                    && policy_input.semantic_edge_count == 4
                    && policy_input.swap_edges.is_empty())
                    || (!is_lane
                        && match index {
                            11 => validate_swap_slice(&policy_input, 1).is_ok(),
                            12 => validate_swap_slice(&policy_input, 1).is_ok(),
                            13 => validate_swap_slice(&policy_input, 4).is_ok(),
                            _ => false,
                        })
            }
        };
        if !valid_shape {
            if mode == CompileMode::PhaseTwo && index >= 11 {
                validate_swap_slice(&policy_input, [1, 1, 4][index - 11])?;
            }
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
            logical_name: None,
            operations: vec![],
            seed: seed.to_string(),
            policy: policy.to_string(),
            semantic_edge_count: policy_input.semantic_edge_count,
            constraint_count,
            constraints,
            swap_edges: policy_input.swap_edges,
            create_packet_bytes,
            update_packet_bytes,
            create_packet: None,
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
        packing: None,
        swap: None,
        packet_measurements: vec![],
        policies,
    })
}

fn main() -> Result<(), String> {
    let args = std::env::args().skip(1).collect::<Vec<_>>();
    let mode = match args.as_slice() {
        [] => CompileMode::PhaseTwo,
        [flag] if flag == "--phase1" => CompileMode::PhaseOne,
        [flag] if flag == "--phase1-forward-jupiter-rollover" => {
            CompileMode::PhaseOneForwardRollover
        }
        _ => return Err(
            "usage: compile-backyard-rwa-resolved-policies [--phase1|--phase1-forward-jupiter-rollover]"
                .to_owned(),
        ),
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
            operation: None,
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

    fn key_for(byte: u8) -> String {
        Pubkey::new_from_array([byte; 32]).to_string()
    }

    fn swap_slice(
        name: &str,
        constraint_count: usize,
    ) -> (Vec<ConstraintInput>, Vec<SwapEdgeInput>) {
        let authority = key_for(200);
        let asset = |symbol: &str, field: &str| {
            let sum = symbol
                .bytes()
                .fold(0u8, |sum, value| sum.wrapping_add(value));
            let kind = field
                .bytes()
                .fold(0u8, |sum, value| sum.wrapping_add(value));
            key_for(sum.wrapping_add(kind))
        };
        let token_program = |symbol: &str| match symbol {
            "USDG" | "PYUSD" => key_for(202),
            _ => key_for(201),
        };
        let edges = expected_swap_edges(name).expect("known swap slice");
        let rows = edges
            .iter()
            .enumerate()
            .map(|(index, key)| {
                let (from, to) = key.split_once("->").expect("edge shape");
                let constraint_index = match constraint_count {
                    1 => 0,
                    4 => ["USDC", "USDG", "USDS", "PYUSD"]
                        .iter()
                        .position(|symbol| *symbol == from)
                        .expect("stable source"),
                    _ => panic!("unsupported test slice"),
                };
                let _ = index;
                SwapEdgeInput {
                    from: from.to_owned(),
                    to: to.to_owned(),
                    constraint_index,
                    authority_index: 0,
                    source_index: 1,
                    destination_index: 2,
                    source_mint_index: 3,
                    destination_mint_index: 4,
                    source_token_program_index: 5,
                    destination_token_program_index: 6,
                    authority: authority.clone(),
                    source_custody: asset(from, "custody"),
                    destination_custody: asset(to, "custody"),
                    source_mint: asset(from, "mint"),
                    destination_mint: asset(to, "mint"),
                    source_token_program: token_program(from),
                    destination_token_program: token_program(to),
                }
            })
            .collect::<Vec<_>>();
        let constraints = (0..constraint_count)
            .map(|constraint_index| {
                let selected = rows
                    .iter()
                    .filter(|edge| edge.constraint_index == constraint_index);
                let mut accounts = vec![AccountConstraintInput {
                    index: 0,
                    pubkeys: vec![authority.clone()],
                }];
                for (index, values) in [
                    (
                        1,
                        selected
                            .clone()
                            .map(|edge| edge.source_custody.clone())
                            .collect::<Vec<_>>(),
                    ),
                    (
                        2,
                        selected
                            .clone()
                            .map(|edge| edge.destination_custody.clone())
                            .collect::<Vec<_>>(),
                    ),
                    (
                        3,
                        selected
                            .clone()
                            .map(|edge| edge.source_mint.clone())
                            .collect::<Vec<_>>(),
                    ),
                    (
                        4,
                        selected
                            .clone()
                            .map(|edge| edge.destination_mint.clone())
                            .collect::<Vec<_>>(),
                    ),
                    (
                        5,
                        selected
                            .clone()
                            .map(|edge| edge.source_token_program.clone())
                            .collect::<Vec<_>>(),
                    ),
                    (
                        6,
                        selected
                            .map(|edge| edge.destination_token_program.clone())
                            .collect::<Vec<_>>(),
                    ),
                ] {
                    let mut unique = values;
                    unique.sort();
                    unique.dedup();
                    accounts.push(AccountConstraintInput {
                        index,
                        pubkeys: unique,
                    });
                }
                ConstraintInput {
                    operation: None,
                    program_id: key_for(199),
                    account_pubkeys: accounts,
                    data: vec![DataConstraintInput::SliceEquals {
                        offset: 0,
                        value_hex: "0102030405060708".to_owned(),
                    }],
                }
            })
            .collect();
        (constraints, rows)
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
            policy_seed_before: "66".to_owned(),
            settings_context_slot: Some(123),
            settings_data_sha256: Some("aa".repeat(32)),
            measurement_blockhash: None,
            measurement_blockhash_slot: None,
            pending_swap: None,
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
                        swap_slice(*name, [1, 1, 4][index - 11]).0
                    },
                    swap_edges: if index < 11 {
                        vec![]
                    } else {
                        swap_slice(*name, [1, 1, 4][index - 11]).1
                    },
                })
                .collect(),
        }
    }

    fn phase_two_input(ready: bool) -> Input {
        let mut value = input(ready);
        value.swap_headers_resolved = false;
        value.measurement_blockhash = Some(Hash::new_unique().to_string());
        value.measurement_blockhash_slot = Some(512);
        value.settings_context_slot = Some(1_024);
        value.pending_swap = Some(PendingSwapInput {
            required_edge_count: 52,
            resolved_edge_count: 0,
            reason: "exact Jupiter headers are pending".to_owned(),
        });
        value.policies = PHASE_TWO_LANE_NAMES
            .iter()
            .enumerate()
            .map(|(lane_index, name)| PolicyInput {
                name: (*name).to_owned(),
                semantic_edge_count: 4,
                constraints: ["deposit", "borrow", "repay", "withdraw"]
                    .iter()
                    .enumerate()
                    .map(|(operation_index, operation)| ConstraintInput {
                        operation: Some((*operation).to_owned()),
                        ..constraint((lane_index * 4 + operation_index + 1) as u8)
                    })
                    .collect(),
                swap_edges: vec![],
            })
            .collect();
        value
    }

    fn phase_two_swap_input() -> Vec<PolicyInput> {
        [
            ("swap/stable-to-rwa", 1usize),
            ("swap/rwa-to-stable", 1usize),
            ("swap/stable-to-stable", 4usize),
        ]
        .into_iter()
        .flat_map(|(slice, count)| {
            let (constraints, edges) = swap_slice(slice, count);
            edges
                .into_iter()
                .map(move |mut edge| {
                    let constraint = constraints[edge.constraint_index].clone();
                    edge.constraint_index = 0;
                    PolicyInput {
                        name: format!("{slice}/{}->{}", edge.from, edge.to),
                        semantic_edge_count: 1,
                        constraints: vec![constraint],
                        swap_edges: vec![edge],
                    }
                })
                .collect::<Vec<_>>()
        })
        .collect()
    }

    #[test]
    fn phase_two_accepts_a_dynamic_confirmed_seed_before_signing() {
        for seed in ["66", "123"] {
            let mut value = phase_two_input(true);
            value.policy_seed_before = seed.to_owned();
            assert!(matches!(
                compile(value, "33".repeat(32), CompileMode::PhaseTwo),
                Err(error) if error.contains("SOLANA_TESTING_PK is required")
            ));
        }
    }

    #[test]
    fn phase_two_packing_attempts_are_complete_and_never_drop_a_kamino_operation() {
        let logical = phase_two_lane_logical_policies(&phase_two_input(true).policies)
            .expect("exact fixture lanes");
        let rungs = phase_two_rungs(&logical);
        assert_eq!(
            rungs
                .iter()
                .map(|(_, policies)| policies.len())
                .collect::<Vec<_>>(),
            vec![11, 22, 22, 33, 22, 33, 33, 44]
        );
        for (_, policies) in rungs {
            assert_eq!(
                policies
                    .iter()
                    .map(|policy| policy.constraints.len())
                    .sum::<usize>(),
                44
            );
        }
    }

    #[test]
    fn phase_two_kamino_partition_search_never_factors_operation_allowlists() {
        let logical = phase_two_lane_logical_policies(&phase_two_input(true).policies)
            .expect("exact fixture lanes");
        let partitions = kamino_partitions(&logical[0]);
        assert_eq!(partitions.len(), 8);
        for (_, groups) in partitions {
            assert_eq!(
                groups
                    .iter()
                    .map(|group| group.constraints.len())
                    .sum::<usize>(),
                4
            );
            // Each policy retains whole original constraints.  No group is a
            // synthesized account/data allowlist that could admit a cross-product.
            for group in groups {
                assert_eq!(group.constraints.len(), group.operations.len());
                assert!(group.operations.iter().all(|operation| matches!(
                    operation.as_str(),
                    "deposit" | "borrow" | "repay" | "withdraw"
                )));
            }
        }
    }

    #[test]
    fn phase_two_swap_input_requires_the_exact_52_edge_bijection_before_packing() {
        let swaps = phase_two_swap_input();
        let logical = phase_two_swap_logical_policies(&swaps).expect("exact 52 swap edges");
        assert_eq!(logical.len(), 52);
        assert_eq!(
            phase_two_swap_rungs(&logical)
                .iter()
                .map(|(_, policies)| policies.len())
                .collect::<Vec<_>>(),
            vec![3, 9, 52]
        );
    }

    #[test]
    fn phase_two_swap_rejects_a_metadata_edge_not_pinned_by_its_constraint() {
        let mut swaps = phase_two_swap_input();
        swaps[0].swap_edges[0].source_mint = key_for(250);
        assert!(matches!(phase_two_swap_logical_policies(&swaps),
            Err(error) if error.contains("is not pinned to its source mint")));
    }

    #[test]
    fn swap_packing_concatenates_whole_exact_constraints_without_cross_products() {
        let logical =
            phase_two_swap_logical_policies(&phase_two_swap_input()).expect("exact 52 swap edges");
        let packed = merge_swap_bin(0, &logical[..2]);
        assert!(
            packed.constraints
                == vec![
                    logical[0].constraints[0].clone(),
                    logical[1].constraints[0].clone(),
                ]
        );
        let mut expected_edges = vec![
            logical[0].swap_edges[0].clone(),
            logical[1].swap_edges[0].clone(),
        ];
        expected_edges[1].constraint_index = 1;
        assert!(packed.swap_edges == expected_edges);
        validate_packed_swap_policy(&packed).expect("both exact edge bindings survive packing");
    }

    #[test]
    fn all_52_compiled_swap_edge_bindings_rebase_to_their_exact_constraints() {
        let logical =
            phase_two_swap_logical_policies(&phase_two_swap_input()).expect("exact 52 swap edges");
        let packed = logical
            .chunks(2)
            .enumerate()
            .map(|(index, members)| merge_swap_bin(index, members))
            .collect::<Vec<_>>();
        assert_eq!(packed.len(), 26);
        assert_eq!(
            packed
                .iter()
                .map(|policy| policy.constraints.len())
                .sum::<usize>(),
            52
        );
        assert_eq!(
            packed
                .iter()
                .map(|policy| policy.swap_edges.len())
                .sum::<usize>(),
            52
        );
        for policy in &packed {
            assert_eq!(
                policy
                    .swap_edges
                    .iter()
                    .map(|edge| edge.constraint_index)
                    .collect::<Vec<_>>(),
                vec![0, 1]
            );
            validate_packed_swap_policy(policy)
                .expect("each authority/custody/mint/program remains pinned to its own constraint");
        }
    }

    #[test]
    fn activation_prefix_places_funding_swaps_before_all_five_market_deposits() {
        let lanes = phase_two_lane_logical_policies(&phase_two_input(true).policies)
            .expect("exact fixture lanes");
        let singles = phase_two_rungs(&lanes)
            .last()
            .expect("single-operation rung")
            .1
            .clone();
        let mut selected = singles;
        selected.extend(
            phase_two_swap_logical_policies(&phase_two_swap_input()).expect("exact swap edges"),
        );
        let (ordered, prefix) = activation_prefix_order(selected).expect("activation prefix");
        assert_eq!(prefix.len(), 10);
        assert!(has_swap_edge(&ordered[0], "USDC", "ONyc"));
        assert_eq!(ordered[1].name, "lane/OnRe/ONyc/USDC/deposit");
        assert_eq!(ordered[2].name, "lane/Prime/PRIME/USDC/deposit");
        assert!(has_swap_edge(&ordered[3], "USDC", "syrupUSDC"));
        assert_eq!(ordered[4].name, "lane/Maple/syrupUSDC/USDC/deposit");
        assert_eq!(ordered[5].name, "lane/AUTO/AUTO/PYUSD/deposit");
        assert!(has_swap_edge(&ordered[6], "USDC", "USDe"));
        assert_eq!(ordered[7].name, "lane/Ethena/USDe/PYUSD/deposit");
        assert!(has_swap_edge(&ordered[8], "PRIME", "USDC"));
        assert!(has_swap_edge(&ordered[9], "USDC", "USDG"));
        assert_eq!(ordered.len(), 96);
    }

    #[test]
    fn phase_two_rejects_missing_confirmed_settings_evidence() {
        let mut value = phase_two_input(true);
        value.settings_context_slot = None;
        assert!(matches!(
            compile(value, "33".repeat(32), CompileMode::PhaseTwo),
            Err(error) if error.contains("requires the confirmed Settings observation slot")
        ));
    }

    #[test]
    fn phase_two_rejects_unlabeled_or_reordered_kamino_operations() {
        let mut value = phase_two_input(true);
        value.policies[0].constraints[0].operation = Some("withdraw".to_owned());
        assert!(matches!(
            compile(value, "33".repeat(32), CompileMode::PhaseTwo),
            Err(error) if error.contains("operations must be deposit, borrow, repay, withdraw")
        ));
    }

    #[test]
    fn unresolved_graph_never_reaches_the_measurement_signer() {
        let result = compile(
            phase_two_input(false),
            "33".repeat(32),
            CompileMode::PhaseTwo,
        );
        assert!(matches!(result, Err(error) if error.contains("Kamino readiness")));
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
                swap_edges: vec![],
            })
            .collect();
        let output = compile(value, "44".repeat(32), CompileMode::PhaseOne)
            .expect("phase one input compiles");
        assert_eq!(output.phase, "phase1");
        assert_eq!(output.policies.len(), 5);
        assert_eq!(output.policies[0].seed, "73");
        assert_eq!(output.policies[4].seed, "77");
    }

    #[test]
    fn phase_one_forward_rollover_compiles_exactly_one_next_seed_policy() {
        let mut value = input(true);
        value.policy_seed_before = PHASE_ONE_FORWARD_ROLLOVER_SEED_BEFORE.to_string();
        value.policies = vec![PolicyInput {
            name: PHASE_ONE_FORWARD_ROLLOVER_NAMES[0].to_owned(),
            semantic_edge_count: 2,
            constraints: vec![constraint(1), constraint(2)],
            swap_edges: vec![],
        }];
        let output = compile(value, "55".repeat(32), CompileMode::PhaseOneForwardRollover)
            .expect("forward rollover compiles");
        assert_eq!(output.phase, "phase1-forward-jupiter-rollover");
        assert_eq!(output.policies.len(), 1);
        assert_eq!(output.policies[0].seed, "66");
        assert_eq!(output.policies[0].semantic_edge_count, 2);
        assert_eq!(output.policies[0].constraint_count, 2);
        assert!(output.policies[0].create_packet_bytes <= PACKET_LIMIT);
    }
}
