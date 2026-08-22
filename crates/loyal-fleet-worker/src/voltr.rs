//! Fail-closed admission for the Backyard Voltr manager route.
//!
//! This module stops at the canonical instruction and generic signed-submission
//! boundary. It never submits a transaction or creates a second queue.

use std::str::FromStr;

use bincode;
use loyal_actions::autonomous_vaults::{
    embedded_backyard_voltr_route_bundle, BackyardVoltrManagerOperation, BackyardVoltrRouteBundle,
    BackyardVoltrStrategy,
};
use loyal_yield_orchestrator::fleet_orchestration::RebalanceOpportunityRecord;
use loyal_yield_orchestrator::{
    fleet_orchestration::{
        RebalanceOpportunityClaimKind, RebalanceOpportunityLease, RouteFeePayerKind,
        SignedRouteSubmissionInput,
    },
    solana_testing_keypair_from_env, NeonSqlClient,
};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_account_decoder_client_types::UiAccountEncoding;
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcAccountInfoConfig, RpcSimulateTransactionConfig},
};
use solana_sdk::{
    address_lookup_table::state::AddressLookupTable,
    commitment_config::CommitmentConfig,
    hash::Hash,
    instruction::Instruction,
    message::{v0, AddressLookupTableAccount, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::Signer,
    transaction::VersionedTransaction,
};

pub(crate) const VOLTR_KAMINO_KIND: &str = "voltr_kamino";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum VoltrOperation {
    Deposit,
    Withdraw,
}

impl VoltrOperation {
    fn parse(value: &str) -> Result<Self, String> {
        match value {
            "deposit" => Ok(Self::Deposit),
            "withdraw" => Ok(Self::Withdraw),
            other => Err(format!("Voltr operation {other:?} is not admitted")),
        }
    }

    fn manager_operation(self) -> BackyardVoltrManagerOperation {
        match self {
            Self::Deposit => BackyardVoltrManagerOperation::Deposit,
            Self::Withdraw => BackyardVoltrManagerOperation::Withdraw,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum VoltrStrategy {
    Main,
    Onre,
    Prime,
    Maple,
}

impl VoltrStrategy {
    fn parse(value: &str) -> Result<Self, String> {
        match value {
            "main" => Ok(Self::Main),
            "onre" => Ok(Self::Onre),
            "prime" => Ok(Self::Prime),
            "maple" => Ok(Self::Maple),
            other => Err(format!("Voltr strategy {other:?} is not admitted")),
        }
    }

    fn manager_strategy(self) -> BackyardVoltrStrategy {
        match self {
            Self::Main => BackyardVoltrStrategy::Main,
            Self::Onre => BackyardVoltrStrategy::Onre,
            Self::Prime => BackyardVoltrStrategy::Prime,
            Self::Maple => BackyardVoltrStrategy::Maple,
        }
    }
}

/// The result of pure Voltr route admission. The worker must hand this exact
/// instruction to the generic signed-submission lifecycle; there is no send
/// method here by design.
#[derive(Clone, Debug)]
#[allow(dead_code)]
pub(crate) struct PreparedVoltrOperation {
    pub instruction: Instruction,
    pub route_id: String,
    pub route_spec_sha256: String,
    pub route_bundle_sha256: String,
    pub strategy: VoltrStrategy,
    pub operation: VoltrOperation,
    pub amount_raw: u64,
    pub vault: Pubkey,
    pub manager: Pubkey,
    pub guardian: Pubkey,
    pub policy: Pubkey,
    pub protected_context_slot: u64,
    pub protected_state_sha256: String,
    pub protected_address_set_sha256: String,
    pub intent_sha256: String,
    pub lookup_table: AddressLookupTableAccount,
    pub lookup_table_authority: Pubkey,
    pub lookup_table_address_count: usize,
    pub lookup_table_ordered_addresses_sha256: String,
    pub alt_requirements_fingerprint: String,
    pub alt_selection_fingerprint: String,
    pub alt_mutation_epochs: Value,
    pub recent_blockhash: Hash,
    pub last_valid_block_height: i64,
}

#[derive(Debug)]
pub(crate) enum RunResult {
    Ready,
    SubmissionQueued {
        signature: String,
        submission_id: i64,
    },
}

pub(crate) fn is_voltr_plan(plan: &Value) -> bool {
    plan.get("kind").and_then(Value::as_str) == Some(VOLTR_KAMINO_KIND)
}

/// Validate every caller-controlled field against the embedded four-market
/// bundle and reconstruct the manager ProgramInteraction wrapper. This is
/// deliberately pure: no RPC, database, signer, or broadcast is reachable.
pub(crate) fn prepare(
    opportunity: &RebalanceOpportunityRecord,
    delegated_signer: Option<&str>,
) -> Result<Option<PreparedVoltrOperation>, String> {
    let plan = &opportunity.execution_plan;
    if !is_voltr_plan(plan) {
        return Ok(None);
    }

    let bundle = embedded_backyard_voltr_route_bundle()
        .map_err(|error| format!("embedded Voltr route bundle rejected: {error}"))?;
    require_eq("cluster", &opportunity.cluster, &bundle.cluster)?;

    let route_id = required_string(plan, "route_id")?;
    require_eq("route_id", &route_id, &bundle.route_id)?;
    let route_spec_sha256 = required_fingerprint(plan, "route_spec_sha256")?;
    require_eq(
        "route_spec_sha256",
        &route_spec_sha256,
        &bundle.route_spec_sha256,
    )?;
    let route_bundle_sha256 = required_fingerprint(plan, "route_bundle_sha256")?;
    require_eq(
        "route_bundle_sha256",
        &route_bundle_sha256,
        &bundle.route_bundle_sha256,
    )?;

    let route_fingerprint = required_fingerprint(plan, "route_fingerprint")?;
    require_eq(
        "route_fingerprint",
        &route_fingerprint,
        &bundle.route_bundle_sha256,
    )?;
    require_optional_eq(
        "route_fingerprint",
        opportunity.route_fingerprint.as_deref(),
        &route_fingerprint,
    )?;
    let strategy = VoltrStrategy::parse(&required_string(plan, "strategy_id")?)?;
    let operation = VoltrOperation::parse(&required_string(plan, "operation")?)?;
    let requirements_fingerprint =
        bundle.requirements_fingerprint(strategy.manager_strategy(), operation.manager_operation());
    require_optional_eq(
        "requirements_fingerprint",
        opportunity.requirements_fingerprint.as_deref(),
        &requirements_fingerprint,
    )?;

    let manager = parse_pubkey(plan, "manager")?;
    let guardian = parse_pubkey(plan, "guardian")?;
    let vault = parse_pubkey(plan, "vault")?;
    require_eq("manager", &manager.to_string(), &bundle.manager.to_string())?;
    require_eq(
        "guardian",
        &guardian.to_string(),
        &bundle.guardian.to_string(),
    )?;
    require_eq("vault", &vault.to_string(), &bundle.vault.to_string())?;
    if delegated_signer.is_some_and(|signer| signer != bundle.guardian.to_string()) {
        return Err(format!(
            "Voltr route is dark: mounted signer is not the exact guardian {}",
            bundle.guardian
        ));
    }

    let amount_raw = required_positive_u64(plan, "amount_raw")?;
    if amount_raw > bundle.max_operation_amount_raw {
        return Err(format!(
            "Voltr amount {amount_raw} exceeds embedded manager cap {}",
            bundle.max_operation_amount_raw
        ));
    }
    let embedded_cap = required_positive_u64(plan, "max_operation_amount_raw")?;
    if embedded_cap != bundle.max_operation_amount_raw {
        return Err("Voltr manager cap does not match embedded route bundle".to_owned());
    }
    if opportunity.amount_raw
        != i64::try_from(amount_raw).map_err(|_| "Voltr amount overflows i64")?
    {
        return Err("Voltr plan amount does not match leased opportunity amount".to_owned());
    }

    let source_kind = required_string(plan, "source_kind")?;
    let target_kind = required_string(plan, "target_kind")?;
    let template = bundle.template(strategy.manager_strategy(), operation.manager_operation());
    match operation {
        VoltrOperation::Deposit => {
            require_eq("source_kind", &source_kind, "voltr_idle")?;
            require_eq("target_kind", &target_kind, "voltr_strategy")?;
            require_eq(
                "target_reserve",
                &opportunity.target_reserve,
                &template.reserve.to_string(),
            )?;
        }
        VoltrOperation::Withdraw => {
            require_eq("source_kind", &source_kind, "voltr_strategy")?;
            require_eq("target_kind", &target_kind, "voltr_idle")?;
            require_eq(
                "source_reserve",
                opportunity
                    .source_reserve
                    .as_deref()
                    .ok_or_else(|| "Voltr withdrawal is missing its source reserve".to_owned())?,
                &template.reserve.to_string(),
            )?;
        }
    }

    let protected_context_slot = required_positive_u64(plan, "protected_context_slot")?;
    let protected_state_sha256 = required_fingerprint(plan, "protected_state_sha256")?;
    let protected_address_set_sha256 = required_fingerprint(plan, "protected_address_set_sha256")?;
    let intent_sha256 = required_fingerprint(plan, "intent_sha256")?;
    let receipt_set_fingerprint = required_fingerprint(plan, "receipt_set_fingerprint")?;
    require_eq(
        "intent_sha256",
        &intent_sha256,
        &bundle.manager_intent_sha256(
            strategy.manager_strategy(),
            operation.manager_operation(),
            amount_raw,
            protected_context_slot,
            &receipt_set_fingerprint,
            &protected_state_sha256,
            &protected_address_set_sha256,
        ),
    )?;

    let instruction = bundle
        .manager_instruction(
            strategy.manager_strategy(),
            operation.manager_operation(),
            amount_raw,
        )
        .map_err(|error| format!("canonical Voltr manager wrapper rejected: {error}"))?;
    let rebuilt = bundle
        .manager_instruction(
            strategy.manager_strategy(),
            operation.manager_operation(),
            amount_raw,
        )
        .map_err(|error| {
            format!("canonical Voltr manager wrapper could not be rebuilt: {error}")
        })?;
    if instruction != rebuilt {
        return Err("Voltr manager wrapper was not deterministic".to_owned());
    }
    let canonical = &template.canonical_manager_instruction;
    if instruction.program_id != canonical.program_id
        || instruction.accounts != canonical.accounts
        || instruction.data.len() != canonical.data.len()
    {
        return Err(
            "Voltr manager wrapper account/program shape drifted from the embedded SDK packet"
                .to_owned(),
        );
    }

    let lookup_table_address = bundle.lookup_table;
    let alt_requirements_fingerprint = requirements_fingerprint.clone();
    let alt_selection_fingerprint = stable_fingerprint(&[
        &requirements_fingerprint,
        "reusable",
        &lookup_table_address.to_string(),
        bundle.lookup_table_ordered_addresses_sha256,
        &bundle.lookup_table_address_count.to_string(),
    ]);
    let alt_mutation_epochs = json!({
        "routeBundleSha256": bundle.route_bundle_sha256,
        "lookupTable": lookup_table_address.to_string(),
        "lookupTableOrderedAddressesSha256": bundle.lookup_table_ordered_addresses_sha256,
        "lookupTableAddressCount": bundle.lookup_table_address_count,
    });
    Ok(Some(PreparedVoltrOperation {
        instruction,
        route_id,
        route_spec_sha256,
        route_bundle_sha256,
        strategy,
        operation,
        amount_raw,
        vault: bundle.vault,
        manager,
        guardian,
        policy: template.policy,
        protected_context_slot,
        protected_state_sha256,
        protected_address_set_sha256,
        intent_sha256,
        lookup_table: AddressLookupTableAccount {
            key: lookup_table_address,
            addresses: Vec::new(),
        },
        lookup_table_authority: bundle.lookup_table_authority,
        lookup_table_address_count: bundle.lookup_table_address_count,
        lookup_table_ordered_addresses_sha256: bundle
            .lookup_table_ordered_addresses_sha256
            .to_owned(),
        alt_requirements_fingerprint,
        alt_selection_fingerprint,
        alt_mutation_epochs,
        recent_blockhash: Hash::default(),
        last_valid_block_height: 0,
    }))
}

/// Revalidate and, in execute mode, atomically persist the exact signed
/// transaction. This function never calls `send_transaction`; the existing
/// sender/confirmer owns publication and recovery.
pub(crate) async fn run(
    rpc: &RpcClient,
    client: &NeonSqlClient,
    lease: &RebalanceOpportunityLease,
    execute: bool,
) -> Result<RunResult, String> {
    if lease.claim_kind != RebalanceOpportunityClaimKind::Execute && execute {
        return Err("Voltr execute requires an execute opportunity lease".to_owned());
    }
    // Revalidation must not touch a mounted secret. Execute loads the
    // guardian key only after the pinned ALT and unsigned simulation gates.
    let mut prepared = prepare(&lease.opportunity, None)?
        .ok_or_else(|| "Voltr worker was asked to run a non-Voltr opportunity".to_owned())?;
    let minimum_context_slot = prepared.protected_context_slot;
    let alt_response = rpc
        .get_account_with_config(
            &prepared.lookup_table.key,
            RpcAccountInfoConfig {
                encoding: Some(UiAccountEncoding::Base64),
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(minimum_context_slot),
                ..RpcAccountInfoConfig::default()
            },
        )
        .map_err(|error| format!("Voltr ALT read failed: {error}"))?;
    if alt_response.context.slot < minimum_context_slot {
        return Err("Voltr ALT read returned an older context slot".to_owned());
    }
    let alt_account = alt_response
        .value
        .ok_or_else(|| "Voltr pinned ALT account is absent".to_owned())?;
    if alt_account.owner != solana_sdk::address_lookup_table::program::id() {
        return Err("Voltr pinned ALT has the wrong owner".to_owned());
    }
    let table = AddressLookupTable::deserialize(&alt_account.data)
        .map_err(|error| format!("Voltr pinned ALT failed to decode: {error}"))?;
    if table.meta.authority != Some(prepared.lookup_table_authority) {
        return Err("Voltr pinned ALT authority drifted".to_owned());
    }
    if table.meta.deactivation_slot != u64::MAX {
        return Err("Voltr pinned ALT is deactivating or deactivated".to_owned());
    }
    let chain_addresses = table.addresses.to_vec();
    // Keep this identical to the route-planner binding: ordered base58
    // addresses are length-prefixed before hashing. Hashing raw pubkey bytes
    // would accept a different digest than the pinned route bundle.
    let mut chain_hasher = Sha256::new();
    for address in &chain_addresses {
        let encoded = address.to_string();
        chain_hasher.update((encoded.len() as u64).to_le_bytes());
        chain_hasher.update(encoded.as_bytes());
    }
    let chain_hash = format!("{:x}", chain_hasher.finalize());
    if chain_hash != prepared.lookup_table_ordered_addresses_sha256 {
        return Err("Voltr pinned ALT ordered-address hash drifted".to_owned());
    }
    if chain_addresses.len() != prepared.lookup_table_address_count {
        return Err("Voltr pinned ALT address count drifted".to_owned());
    }
    prepared.lookup_table.addresses = chain_addresses;

    let (recent_blockhash, last_valid_block_height) = rpc
        .get_latest_blockhash_with_commitment(CommitmentConfig::confirmed())
        .map_err(|error| format!("Voltr blockhash read failed: {error}"))?;
    prepared.recent_blockhash = recent_blockhash;
    prepared.last_valid_block_height = i64::try_from(last_valid_block_height)
        .map_err(|_| "Voltr last valid block height overflowed i64".to_owned())?;
    let message = v0::Message::try_compile(
        &prepared.guardian,
        &[prepared.instruction.clone()],
        &[prepared.lookup_table.clone()],
        recent_blockhash,
    )
    .map_err(|error| format!("Voltr v0 message compilation failed: {error}"))?;
    let null_signer = solana_sdk::signer::null_signer::NullSigner::new(&prepared.guardian);
    let null_transaction =
        VersionedTransaction::try_new(VersionedMessage::V0(message.clone()), &[&null_signer])
            .map_err(|error| format!("Voltr NullSigner transaction build failed: {error}"))?;
    let packet_size = bincode::serialize(&null_transaction)
        .map_err(|error| format!("Voltr packet serialization failed: {error}"))?
        .len();
    if packet_size > PACKET_DATA_SIZE {
        return Err(format!(
            "Voltr packet is {packet_size} bytes; packet limit is {PACKET_DATA_SIZE}"
        ));
    }
    let simulation = rpc
        .simulate_transaction_with_config(
            &null_transaction,
            RpcSimulateTransactionConfig {
                sig_verify: false,
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(minimum_context_slot),
                ..RpcSimulateTransactionConfig::default()
            },
        )
        .map_err(|error| format!("Voltr NullSigner simulation failed: {error}"))?;
    if let Some(error) = simulation.value.err {
        return Err(format!("Voltr NullSigner simulation rejected: {error:?}"));
    }
    if !execute {
        return Ok(RunResult::Ready);
    }

    let signer = solana_testing_keypair_from_env()
        .map_err(|error| format!("Voltr guardian signer load failed: {error}"))?;
    if signer.pubkey() != prepared.guardian {
        return Err(format!(
            "Voltr guardian signer mismatch: expected {}, got {}",
            prepared.guardian,
            signer.pubkey()
        ));
    }
    let signed = VersionedTransaction::try_new(VersionedMessage::V0(message), &[&signer])
        .map_err(|error| format!("Voltr signed transaction build failed: {error}"))?;
    let signed_size = bincode::serialize(&signed)
        .map_err(|error| format!("Voltr signed packet serialization failed: {error}"))?
        .len();
    if signed_size > PACKET_DATA_SIZE {
        return Err(format!(
            "Voltr signed packet is {signed_size} bytes; packet limit is {PACKET_DATA_SIZE}"
        ));
    }
    let fee_message = match &signed.message {
        VersionedMessage::V0(message) => message,
        VersionedMessage::Legacy(_) => {
            return Err("Voltr signed transaction unexpectedly used a legacy message".to_owned())
        }
    };
    let fee = rpc
        .get_fee_for_message(fee_message)
        .map_err(|error| format!("Voltr fee read failed: {error}"))?;
    if i64::try_from(fee).map_err(|_| "Voltr fee overflowed i64".to_owned())?
        > lease.opportunity.estimated_cost_lamports
    {
        return Err("Voltr compiled fee exceeds the opportunity fee cap".to_owned());
    }
    let signed_simulation = rpc
        .simulate_transaction_with_config(
            &signed,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(minimum_context_slot),
                ..RpcSimulateTransactionConfig::default()
            },
        )
        .map_err(|error| format!("Voltr signed simulation failed: {error}"))?;
    if let Some(error) = signed_simulation.value.err {
        return Err(format!("Voltr signed simulation rejected: {error:?}"));
    }

    let writable = writable_message_keys(&signed.message, &prepared.lookup_table);
    let selected_reserve = match prepared.operation {
        VoltrOperation::Deposit => lease.opportunity.target_reserve.as_str(),
        VoltrOperation::Withdraw => lease
            .opportunity
            .source_reserve
            .as_deref()
            .ok_or_else(|| "Voltr withdrawal is missing its source reserve".to_owned())?,
    };
    let conflict = semantic_conflict_keys(prepared.vault, selected_reserve);
    client
        .acquire_route_account_conflict_leases(lease, &conflict, lease.expires_at)
        .await
        .map_err(|error| format!("Voltr conflict lease acquisition failed: {error}"))?;

    let wire = bincode::serialize(&signed)
        .map_err(|error| format!("Voltr signed wire serialization failed: {error}"))?;
    let message_wire = bincode::serialize(&signed.message)
        .map_err(|error| format!("Voltr message serialization failed: {error}"))?;
    let signature = signed
        .signatures
        .first()
        .ok_or_else(|| "Voltr signed transaction has no signature".to_owned())?
        .to_string();
    let submission = SignedRouteSubmissionInput {
        cluster: lease.opportunity.cluster.clone(),
        semantic_key: format!("fleet-opportunity:{}", lease.opportunity.id),
        opportunity_id: lease.opportunity.id,
        decision_id: None,
        signed_transaction: wire.clone(),
        signed_transaction_hash: format!("{:x}", Sha256::digest(&wire)),
        message_hash: format!("{:x}", Sha256::digest(&message_wire)),
        transaction_signature: signature.clone(),
        recent_blockhash: recent_blockhash.to_string(),
        last_valid_block_height: prepared.last_valid_block_height,
        source_snapshot_id: lease.opportunity.source_snapshot_id,
        optimizer_epoch_id: lease.opportunity.optimizer_epoch_id,
        alt_requirements_fingerprint: prepared.alt_requirements_fingerprint,
        alt_selection_fingerprint: prepared.alt_selection_fingerprint,
        alt_mutation_epochs: prepared.alt_mutation_epochs,
        fee_payer: prepared.guardian.to_string(),
        fee_payer_kind: RouteFeePayerKind::Policy,
        fee_payer_balance_lamports: None,
        fee_payer_balance_slot: None,
        fee_payer_balance_observed_at: None,
        policy_setup_funding_lamports: None,
        compiled_fee_lamports: i64::try_from(fee)
            .map_err(|_| "Voltr fee overflowed i64".to_owned())?,
        writable_account_keys: writable,
        conflict_account_keys: conflict,
        executor_owner: lease.owner.clone(),
        executor_fencing_token: lease.fencing_token,
    };
    let (_, persisted) = client
        .record_voltr_manager_decision_with_signed_submission(lease, submission)
        .await
        .map_err(|error| format!("Voltr atomic signed handoff failed: {error}"))?;
    Ok(RunResult::SubmissionQueued {
        signature,
        submission_id: persisted.id,
    })
}

fn writable_message_keys(
    message: &VersionedMessage,
    lookup_table: &AddressLookupTableAccount,
) -> Vec<String> {
    let VersionedMessage::V0(message) = message else {
        return Vec::new();
    };
    let mut keys = Vec::new();
    for (index, key) in message.account_keys.iter().enumerate() {
        if message.is_maybe_writable(index, None) {
            keys.push(key.to_string());
        }
    }
    for lookup in &message.address_table_lookups {
        for index in &lookup.writable_indexes {
            if let Some(key) = lookup_table.addresses.get(usize::from(*index)) {
                keys.push(key.to_string());
            }
        }
    }
    sorted_unique(keys)
}

fn sorted_unique(mut values: Vec<String>) -> Vec<String> {
    values.sort();
    values.dedup();
    values
}

fn stable_fingerprint(parts: &[&str]) -> String {
    let mut hasher = Sha256::new();
    for part in parts {
        hasher.update((part.len() as u64).to_le_bytes());
        hasher.update(part.as_bytes());
    }
    format!("{:x}", hasher.finalize())
}

fn semantic_conflict_keys(vault: Pubkey, selected_reserve: &str) -> Vec<String> {
    let mut keys = vec![
        format!("voltr:vault:{vault}"),
        format!("kamino:reserve:{selected_reserve}"),
    ];
    keys.sort_unstable();
    keys
}

fn required_string(plan: &Value, field: &str) -> Result<String, String> {
    plan.get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.trim().is_empty())
        .map(ToOwned::to_owned)
        .ok_or_else(|| format!("Voltr execution_plan.{field} is required"))
}

fn required_fingerprint(plan: &Value, field: &str) -> Result<String, String> {
    let value = required_string(plan, field)?;
    if value.len() != 64 || !value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(format!(
            "Voltr execution_plan.{field} must be a 64-character SHA-256 hex digest"
        ));
    }
    Ok(value.to_ascii_lowercase())
}

fn required_positive_u64(plan: &Value, field: &str) -> Result<u64, String> {
    let value = plan
        .get(field)
        .and_then(Value::as_i64)
        .filter(|value| *value > 0)
        .ok_or_else(|| format!("Voltr execution_plan.{field} is required as a positive integer"))?;
    u64::try_from(value).map_err(|_| format!("Voltr execution_plan.{field} must be positive"))
}

fn parse_pubkey(plan: &Value, field: &str) -> Result<Pubkey, String> {
    Pubkey::from_str(&required_string(plan, field)?).map_err(|error| {
        format!("Voltr execution_plan.{field} is not a valid Solana address: {error}")
    })
}

fn require_eq(field: &str, actual: &str, expected: &str) -> Result<(), String> {
    if actual == expected {
        Ok(())
    } else {
        Err(format!(
            "Voltr {field} mismatch: expected {expected}, got {actual}"
        ))
    }
}

fn require_optional_eq(field: &str, actual: Option<&str>, expected: &str) -> Result<(), String> {
    require_eq(
        field,
        actual.ok_or_else(|| format!("Voltr {field} is absent"))?,
        expected,
    )
}

#[allow(dead_code)]
fn _bundle_type_is_pinned(_: &BackyardVoltrRouteBundle) {}
