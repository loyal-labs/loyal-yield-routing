use crate::squads::{
    create_program_interaction_action_instruction, SquadsAccountConstraint,
    SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator, SquadsDataValue,
    SquadsInstructionConstraint,
};
use crate::{
    compile_squads_inner_instruction, derive_action_account, derive_squads_vault,
    execute_program_interaction_policy_instruction, KAMINO_FARMS_PROGRAM_ID,
    KAMINO_LEND_PROGRAM_ID, USDC_MINT,
};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use solana_sdk::{instruction::Instruction, pubkey, pubkey::Pubkey, sysvar};
use std::{fmt, str::FromStr};

pub const VOLTR_VAULT_PROGRAM_ID: Pubkey = pubkey!("vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8");
pub const VOLTR_KAMINO_ADAPTOR_PROGRAM_ID: Pubkey =
    pubkey!("to6Eti9CsC5FGkAtqiPphvKD2hiQiLsS8zWiDBqBPKR");

pub const VOLTR_INITIALIZE_STRATEGY_DISCRIMINATOR: [u8; 8] =
    [208, 119, 144, 145, 178, 57, 105, 252];
pub const VOLTR_DEPOSIT_STRATEGY_DISCRIMINATOR: [u8; 8] = [246, 82, 57, 226, 131, 222, 253, 249];
pub const VOLTR_WITHDRAW_STRATEGY_DISCRIMINATOR: [u8; 8] = [31, 45, 162, 5, 193, 217, 134, 188];
pub const VOLTR_KAMINO_INITIALIZE_MARKET_DISCRIMINATOR: [u8; 8] =
    [35, 35, 189, 193, 155, 48, 170, 203];
pub const VOLTR_KAMINO_DEPOSIT_MARKET_DISCRIMINATOR: [u8; 8] =
    [212, 53, 186, 193, 147, 53, 143, 123];
pub const VOLTR_KAMINO_WITHDRAW_MARKET_DISCRIMINATOR: [u8; 8] =
    [123, 109, 245, 15, 150, 48, 203, 113];

/// The only strategy identities accepted by the Backyard four-market policy
/// catalog.  Keep this allowlist here, next to the byte-level policy builder,
/// so callers cannot accidentally turn the catalog into an arbitrary graph
/// compiler.
pub const BACKYARD_VOLTR_STRATEGY_IDS: [&str; 4] = ["main", "onre", "prime", "maple"];
pub const BACKYARD_VOLTR_STRATEGY_RESERVES: [Pubkey; 4] = [
    pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
    pubkey!("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
    pubkey!("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu"),
    pubkey!("Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo"),
];
pub const BACKYARD_VOLTR_STRATEGY_LENDING_MARKETS: [Pubkey; 4] = [
    pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA"),
    pubkey!("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
];
pub const BACKYARD_VOLTR_STRATEGY_FARMS: [Pubkey; 4] = [
    pubkey!("JAvnB9AKtgPsTEoKmn24Bq64UMoYcrtWtq42HHBdsPkh"),
    pubkey!("GNcywqL6AZajsyyitxGQUvbihPgAzGZUqKfjYcvTj2pi"),
    pubkey!("HqEqwkTmqCAVEQQaEBuSSGD2EAvcorFogqhZz46TYJyz"),
    pubkey!("6Y9fzrWzGZaxdAJ2eWRg9UZpL3kqPDiVXAb67KJpWdUg"),
];

pub const BACKYARD_VOLTR_ROUTE_ID: &str = "loyal-backyard-four-market-usdc-v1";
pub const BACKYARD_VOLTR_ROUTE_SPEC_SHA256: &str =
    "df6547aeaba99f6bf32a0f56d63c50d30f84d7dc1d3df801266b97bd9811e8f4";
pub const BACKYARD_VOLTR_POLICY_ARTIFACT_SHA256: &str =
    "aaf5793545f21f4af0363293b8e2e1ea05812159415a0ff04b382738c0cf07dd";
pub const BACKYARD_VOLTR_POLICY_ARTIFACT_FILE_SHA256: &str =
    "94cba2580b915cf9c93a7bd853701cc1acaafc9b0ab6c83492afbae3cfc209df";
pub const BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS: u64 = 600;
pub const BACKYARD_VOLTR_NORMAL_OPTIMIZATION_INTERVAL_SECONDS: u64 = 3_600;
/// Current route-bundle liquidity floor. The production activation may raise
/// this only by replacing the immutable bundle and its authorization digest.
pub const BACKYARD_VOLTR_CONFIGURED_IDLE_SAFETY_BUFFER_RAW: u64 = 0;
pub const BACKYARD_VOLTR_IDLE_ATA: Pubkey = pubkey!("9LHpTxtFDYb8xJAruX9uTrceohFms2KyRvkXREj3iV9P");
pub const BACKYARD_VOLTR_LP_MINT: Pubkey = pubkey!("dbQkLsUYE7ADHHv8XEottANAa773K4xM4nyPjVdutka");
pub const BACKYARD_VOLTR_LOOKUP_TABLE: Pubkey =
    pubkey!("HSmmBwB7ZRWEsWf4q47w65hXfmqNrfP67KDtpuVrHK7T");
pub const BACKYARD_VOLTR_LOOKUP_TABLE_AUTHORITY: Pubkey =
    pubkey!("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ");
pub const BACKYARD_VOLTR_LOOKUP_TABLE_ADDRESS_COUNT: usize = 185;
pub const BACKYARD_VOLTR_LOOKUP_TABLE_ORDERED_ADDRESSES_SHA256: &str =
    "901173cf1cc0bafa9152c66425eb5a4c05819cbdfa742bc9c489d4fa167157c5";

const BACKYARD_VOLTR_POLICY_ARTIFACT_JSON: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v2.json"
));

const INITIALIZE_ACCOUNT_COUNT: usize = 20;
const DEPOSIT_ACCOUNT_COUNT: usize = 31;
const WITHDRAW_ACCOUNT_COUNT: usize = 28;
const INITIALIZE_DATA_LENGTH: usize = 22;
const MANAGER_OPERATION_DATA_LENGTH: usize = 30;

const INITIALIZE_SECURITY_CRITICAL: &[u8] = &[0, 1, 2, 3, 4, 5, 6, 7, 8, 11, 13, 16, 17, 19];
const DEPOSIT_SECURITY_CRITICAL: &[u8] = &[
    0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15, 17, 21, 29, 30,
];
const WITHDRAW_SECURITY_CRITICAL: &[u8] = &[
    0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 11, 12, 13, 14, 15, 17, 21, 26, 27,
];

const INITIALIZE_METAS: [(bool, bool); INITIALIZE_ACCOUNT_COUNT] = [
    (true, true),
    (true, false),
    (false, false),
    (false, false),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, false),
    (false, true),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, false),
    (false, false),
];

const DEPOSIT_METAS: [(bool, bool); DEPOSIT_ACCOUNT_COUNT] = [
    (true, false),
    (false, false),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, true),
    (false, true),
    (false, false),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, false),
    (false, false),
    (false, false),
];

const WITHDRAW_METAS: [(bool, bool); WITHDRAW_ACCOUNT_COUNT] = [
    (true, false),
    (false, false),
    (false, true),
    (false, false),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, true),
    (false, false),
    (false, true),
    (false, true),
    (false, false),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, true),
    (false, true),
    (false, false),
    (false, false),
    (false, false),
];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VoltrKaminoConstraintProfile {
    /// Pins every account in the generated Voltr instruction. This is the audit
    /// baseline and is expected to be too large for some policy-create packets.
    ExactAllAccounts,
    /// Pins every capital-routing edge and relies on Voltr/Kamino PDA and
    /// reserve validation for the omitted protocol-derived support accounts.
    SecurityCritical,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VoltrKaminoPolicySeeds {
    pub initialize: u64,
    pub deposit: u64,
    pub withdraw: u64,
}

#[derive(Clone, Debug)]
pub struct VoltrKaminoPolicyTemplate {
    pub vault: Pubkey,
    pub reserve: Pubkey,
    pub max_operation_amount_raw: u64,
    pub initialize_instruction: Instruction,
    pub deposit_instruction: Instruction,
    pub withdraw_instruction: Instruction,
}

#[derive(Clone, Debug)]
pub struct VoltrKaminoPolicyPlan {
    pub policy: Pubkey,
    pub policy_seed: u64,
    pub create_instruction: Instruction,
    pub constrained_account_indexes: Vec<u8>,
}

#[derive(Clone, Debug)]
pub struct VoltrKaminoPolicies {
    pub manager: Pubkey,
    pub initialize: VoltrKaminoPolicyPlan,
    pub deposit: VoltrKaminoPolicyPlan,
    pub withdraw: VoltrKaminoPolicyPlan,
}

/// The permanent runtime policy sequence. Strategy initialization is a
/// bootstrap-only admin action and is deliberately absent from this type.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct VoltrKaminoRuntimePolicySeeds {
    pub deposit: u64,
    pub withdraw: u64,
}

#[derive(Clone, Debug)]
pub struct VoltrKaminoRuntimePolicyTemplate {
    pub vault: Pubkey,
    pub reserve: Pubkey,
    pub max_operation_amount_raw: u64,
    pub deposit_instruction: Instruction,
    pub withdraw_instruction: Instruction,
}

#[derive(Clone, Debug)]
pub struct VoltrKaminoRuntimePolicies {
    pub manager: Pubkey,
    pub deposit: VoltrKaminoPolicyPlan,
    pub withdraw: VoltrKaminoPolicyPlan,
}

/// One canonical entry in the fixed four-market runtime catalog.
///
/// The strategy id and reserve are deliberately checked against the product
/// allowlist by [`create_backyard_voltr_runtime_policy_catalog`].  The
/// instructions themselves still go through the existing strict Voltr/Kamino
/// graph validator; this type does not provide a caller-controlled escape
/// hatch for arbitrary account graphs.
#[derive(Clone, Debug)]
pub struct BackyardVoltrRuntimePolicySpec {
    pub strategy_id: &'static str,
    pub seeds: VoltrKaminoRuntimePolicySeeds,
    pub profile: VoltrKaminoConstraintProfile,
    pub template: VoltrKaminoRuntimePolicyTemplate,
}

/// Exactly four strategy pairs, in canonical Main/OnRe/Prime/Maple order.
pub type BackyardVoltrRuntimePolicyCatalog =
    [VoltrKaminoRuntimePolicies; BACKYARD_VOLTR_STRATEGY_IDS.len()];

/// Closed strategy selector for the production Backyard route bundle.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum BackyardVoltrStrategy {
    Main,
    Onre,
    Prime,
    Maple,
}

impl BackyardVoltrStrategy {
    pub const ALL: [Self; 4] = [Self::Main, Self::Onre, Self::Prime, Self::Maple];

    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Main => "main",
            Self::Onre => "onre",
            Self::Prime => "prime",
            Self::Maple => "maple",
        }
    }

    const fn index(self) -> usize {
        match self {
            Self::Main => 0,
            Self::Onre => 1,
            Self::Prime => 2,
            Self::Maple => 3,
        }
    }

    fn parse(value: &str) -> Result<Self, BackyardVoltrBundleError> {
        Self::ALL
            .into_iter()
            .find(|strategy| strategy.as_str() == value)
            .ok_or_else(|| {
                BackyardVoltrBundleError::Invalid(format!(
                    "unknown Backyard Voltr strategy {value}"
                ))
            })
    }
}

/// The only two capital-moving operations admitted by the route bundle.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum BackyardVoltrManagerOperation {
    Deposit,
    Withdraw,
}

impl BackyardVoltrManagerOperation {
    pub const ALL: [Self; 2] = [Self::Deposit, Self::Withdraw];

    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Deposit => "deposit",
            Self::Withdraw => "withdraw",
        }
    }

    fn parse(value: &str) -> Result<Self, BackyardVoltrBundleError> {
        Self::ALL
            .into_iter()
            .find(|operation| operation.as_str() == value)
            .ok_or_else(|| {
                BackyardVoltrBundleError::Invalid(format!(
                    "unknown Backyard Voltr manager operation {value}"
                ))
            })
    }
}

#[derive(Clone, Debug)]
pub struct BackyardVoltrManagerTemplate {
    pub strategy: BackyardVoltrStrategy,
    pub operation: BackyardVoltrManagerOperation,
    pub reserve: Pubkey,
    pub lending_market: Pubkey,
    pub collateral_farm: Pubkey,
    pub strategy_init_receipt: Pubkey,
    pub strategy_asset_ata: Pubkey,
    pub policy_seed: u64,
    pub policy: Pubkey,
    pub inner_instruction: Instruction,
    pub canonical_manager_instruction: Instruction,
}

/// Build-time embedded, closed four-market route bundle.
///
/// The source artifact is generated by the pinned Voltr SDK, while this type
/// independently decodes its inner instructions and reconstructs the Squads
/// ProgramInteraction wrapper. Runtime callers may select only the operation,
/// strategy, and bounded amount; they cannot supply account metas or bytes.
#[derive(Clone, Debug)]
pub struct BackyardVoltrRouteBundle {
    pub route_id: String,
    pub route_spec_sha256: String,
    pub source_artifact_sha256: String,
    pub route_bundle_sha256: String,
    pub cluster: String,
    pub genesis_hash: String,
    pub settings: Pubkey,
    pub manager: Pubkey,
    pub guardian: Pubkey,
    pub vault: Pubkey,
    pub vault_index: u8,
    pub idle_authority: Pubkey,
    pub idle_ata: Pubkey,
    pub lp_mint: Pubkey,
    pub max_operation_amount_raw: u64,
    pub withdrawal_wait_seconds: u64,
    pub normal_optimization_interval_seconds: u64,
    pub configured_idle_safety_buffer_raw: u64,
    pub lookup_table: Pubkey,
    pub lookup_table_authority: Pubkey,
    pub lookup_table_address_count: usize,
    pub lookup_table_ordered_addresses_sha256: &'static str,
    pub packet_limit_bytes: usize,
    pub templates: Vec<BackyardVoltrManagerTemplate>,
}

impl BackyardVoltrRouteBundle {
    pub fn template(
        &self,
        strategy: BackyardVoltrStrategy,
        operation: BackyardVoltrManagerOperation,
    ) -> &BackyardVoltrManagerTemplate {
        self.templates
            .iter()
            .find(|template| template.strategy == strategy && template.operation == operation)
            .expect("validated Backyard Voltr bundle contains all eight templates")
    }

    pub fn manager_instruction(
        &self,
        strategy: BackyardVoltrStrategy,
        operation: BackyardVoltrManagerOperation,
        amount_raw: u64,
    ) -> Result<Instruction, BackyardVoltrBundleError> {
        if amount_raw == 0 || amount_raw > self.max_operation_amount_raw {
            return Err(BackyardVoltrBundleError::AmountOutOfBounds {
                amount_raw,
                maximum_raw: self.max_operation_amount_raw,
            });
        }
        let template = self.template(strategy, operation);
        let mut inner = template.inner_instruction.clone();
        if inner.data.len() != MANAGER_OPERATION_DATA_LENGTH {
            return Err(BackyardVoltrBundleError::Invalid(
                "manager inner instruction length drifted".to_owned(),
            ));
        }
        inner.data[8..16].copy_from_slice(&amount_raw.to_le_bytes());
        let mut transaction_accounts = Vec::new();
        let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner);
        Ok(execute_program_interaction_policy_instruction(
            template.policy,
            self.guardian,
            self.vault_index,
            vec![compiled],
            vec![0],
            transaction_accounts,
        ))
    }

    pub fn requirements_fingerprint(
        &self,
        strategy: BackyardVoltrStrategy,
        operation: BackyardVoltrManagerOperation,
    ) -> String {
        let template = self.template(strategy, operation);
        let mut addresses = template
            .canonical_manager_instruction
            .accounts
            .iter()
            .map(|account| account.pubkey.to_string())
            .collect::<Vec<_>>();
        addresses.sort();
        addresses.dedup();
        sha256_hex(
            format!(
                "{}:{}:{}:{}:{}:{}",
                self.route_bundle_sha256,
                strategy.as_str(),
                operation.as_str(),
                self.lookup_table,
                self.lookup_table_ordered_addresses_sha256,
                addresses.join(":")
            )
            .as_bytes(),
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn manager_intent_sha256(
        &self,
        strategy: BackyardVoltrStrategy,
        operation: BackyardVoltrManagerOperation,
        amount_raw: u64,
        protected_context_slot: u64,
        receipt_set_fingerprint: &str,
        protected_state_sha256: &str,
        protected_address_set_sha256: &str,
    ) -> String {
        sha256_hex(
            format!(
                "{}:{}:{}:{}:{}:{}:{}:{}",
                self.route_bundle_sha256,
                strategy.as_str(),
                operation.as_str(),
                amount_raw,
                protected_context_slot,
                receipt_set_fingerprint,
                protected_state_sha256,
                protected_address_set_sha256
            )
            .as_bytes(),
        )
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BackyardVoltrBundleError {
    Invalid(String),
    AmountOutOfBounds { amount_raw: u64, maximum_raw: u64 },
}

impl fmt::Display for BackyardVoltrBundleError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Invalid(message) => formatter.write_str(message),
            Self::AmountOutOfBounds {
                amount_raw,
                maximum_raw,
            } => write!(
                formatter,
                "Backyard Voltr amount {amount_raw} must be positive and at most {maximum_raw}"
            ),
        }
    }
}

impl std::error::Error for BackyardVoltrBundleError {}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedCatalog {
    schema_version: u64,
    evidence_type: String,
    verdict: String,
    broadcast: bool,
    route_id: String,
    route_spec_sha256: String,
    artifact_sha256: String,
    runtime_policy_count: usize,
    setup_policy_included: bool,
    manager: String,
    policy_seed_before: String,
    policies: Vec<EmbeddedPolicy>,
    source_manifests: Vec<EmbeddedManifest>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedPolicy {
    strategy_id: String,
    operation: String,
    seed: String,
    policy: String,
    manager_execution: EmbeddedWireInstruction,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedManifest {
    strategy_id: Option<String>,
    route_id: String,
    route_spec_sha256: String,
    cluster: String,
    genesis_hash: String,
    ids: EmbeddedManifestIds,
    vault_index: u8,
    limits: EmbeddedLimits,
    policy_seeds: EmbeddedPolicySeeds,
    instructions: EmbeddedInstructions,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedManifestIds {
    squads_settings: String,
    manager: String,
    guardian: String,
    vault: String,
    reserve: String,
    lending_market: String,
    collateral_farm: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedLimits {
    max_per_operation_raw: String,
    solana_packet_bytes: usize,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedPolicySeeds {
    deposit: String,
    withdraw: String,
}

#[derive(Debug, Deserialize)]
struct EmbeddedInstructions {
    deposit: EmbeddedCanonicalInstruction,
    withdraw: EmbeddedCanonicalInstruction,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedCanonicalInstruction {
    program_id: String,
    data_base64: String,
    data_sha256: String,
    data_length: usize,
    accounts: Vec<EmbeddedCanonicalAccount>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedCanonicalAccount {
    index: usize,
    label: String,
    address: String,
    signer: bool,
    writable: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct EmbeddedWireInstruction {
    program_id: String,
    data_base64: String,
    data_sha256: String,
    data_length: usize,
    accounts: Vec<EmbeddedWireAccount>,
}

#[derive(Debug, Deserialize)]
struct EmbeddedWireAccount {
    address: String,
    signer: bool,
    writable: bool,
}

/// Load and independently verify the SDK-generated, compile-time embedded
/// four-market route bundle.
pub fn embedded_backyard_voltr_route_bundle(
) -> Result<BackyardVoltrRouteBundle, BackyardVoltrBundleError> {
    let source_artifact_sha256 = sha256_hex(BACKYARD_VOLTR_POLICY_ARTIFACT_JSON.as_bytes());
    require_bundle(
        source_artifact_sha256 == BACKYARD_VOLTR_POLICY_ARTIFACT_FILE_SHA256,
        "embedded Backyard Voltr policy artifact file hash drifted",
    )?;
    let catalog: EmbeddedCatalog = serde_json::from_str(BACKYARD_VOLTR_POLICY_ARTIFACT_JSON)
        .map_err(|error| {
            BackyardVoltrBundleError::Invalid(format!(
                "embedded Backyard Voltr policy artifact is invalid JSON: {error}"
            ))
        })?;
    require_bundle(
        catalog.schema_version == 1,
        "policy artifact schema must be v1",
    )?;
    require_bundle(
        catalog.evidence_type == "backyard-voltr-runtime-policy-artifact",
        "policy artifact evidence type drifted",
    )?;
    require_bundle(
        catalog.verdict == "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED",
        "policy artifact verdict drifted",
    )?;
    require_bundle(!catalog.broadcast, "policy artifact must be no-broadcast")?;
    require_bundle(
        catalog.route_id == BACKYARD_VOLTR_ROUTE_ID,
        "policy artifact route id drifted",
    )?;
    require_bundle(
        catalog.route_spec_sha256 == BACKYARD_VOLTR_ROUTE_SPEC_SHA256,
        "policy artifact route spec hash drifted",
    )?;
    require_bundle(
        catalog.artifact_sha256 == BACKYARD_VOLTR_POLICY_ARTIFACT_SHA256,
        "policy artifact canonical hash drifted",
    )?;
    require_bundle(
        catalog.runtime_policy_count == 8 && catalog.policies.len() == 8,
        "policy artifact must contain exactly eight runtime policies",
    )?;
    require_bundle(
        !catalog.setup_policy_included,
        "runtime route bundle must not include setup policy",
    )?;
    require_bundle(
        catalog.source_manifests.len() == BackyardVoltrStrategy::ALL.len(),
        "route bundle must contain exactly four source manifests",
    )?;
    let policy_seed_before = parse_u64(&catalog.policy_seed_before, "policySeedBefore")?;

    let manager = parse_pubkey(&catalog.manager, "catalog manager")?;
    let mut settings = None;
    let mut guardian = None;
    let mut vault = None;
    let mut vault_index = None;
    let mut cluster = None;
    let mut genesis_hash = None;
    let mut maximum_raw = None;
    let mut packet_limit_bytes = None;
    let mut idle_authority = None;
    let mut templates = Vec::with_capacity(8);

    for (manifest_index, manifest) in catalog.source_manifests.iter().enumerate() {
        let strategy =
            BackyardVoltrStrategy::parse(manifest.strategy_id.as_deref().ok_or_else(|| {
                BackyardVoltrBundleError::Invalid(
                    "source manifest strategy id is absent".to_owned(),
                )
            })?)?;
        require_bundle(
            strategy.index() == manifest_index,
            "source manifests are not in canonical Main/OnRe/Prime/Maple order",
        )?;
        require_bundle(
            manifest.route_id == catalog.route_id
                && manifest.route_spec_sha256 == catalog.route_spec_sha256,
            "source manifest route binding drifted",
        )?;
        require_equal(&mut cluster, manifest.cluster.clone(), "cluster")?;
        require_equal(
            &mut genesis_hash,
            manifest.genesis_hash.clone(),
            "genesis hash",
        )?;
        let manifest_settings = parse_pubkey(&manifest.ids.squads_settings, "Squads settings")?;
        let manifest_manager = parse_pubkey(&manifest.ids.manager, "manager")?;
        let manifest_guardian = parse_pubkey(&manifest.ids.guardian, "guardian")?;
        let manifest_vault = parse_pubkey(&manifest.ids.vault, "vault")?;
        require_bundle(
            manifest_manager == manager,
            "source manifest manager drifted",
        )?;
        require_equal(&mut settings, manifest_settings, "Squads settings")?;
        require_equal(&mut guardian, manifest_guardian, "guardian")?;
        require_equal(&mut vault, manifest_vault, "vault")?;
        require_equal(&mut vault_index, manifest.vault_index, "vault index")?;
        let manifest_maximum =
            parse_u64(&manifest.limits.max_per_operation_raw, "maxPerOperationRaw")?;
        require_equal(&mut maximum_raw, manifest_maximum, "manager operation cap")?;
        require_equal(
            &mut packet_limit_bytes,
            manifest.limits.solana_packet_bytes,
            "packet limit",
        )?;

        let reserve = parse_pubkey(&manifest.ids.reserve, "reserve")?;
        let lending_market = parse_pubkey(&manifest.ids.lending_market, "lending market")?;
        let collateral_farm = parse_pubkey(&manifest.ids.collateral_farm, "collateral farm")?;
        require_bundle(
            reserve == BACKYARD_VOLTR_STRATEGY_RESERVES[strategy.index()]
                && lending_market == BACKYARD_VOLTR_STRATEGY_LENDING_MARKETS[strategy.index()]
                && collateral_farm == BACKYARD_VOLTR_STRATEGY_FARMS[strategy.index()],
            "source manifest strategy graph drifted",
        )?;

        let deposit_inner = canonical_instruction(&manifest.instructions.deposit)?;
        let withdraw_inner = canonical_instruction(&manifest.instructions.withdraw)?;
        let strategy_init_receipt = labeled_account(
            &manifest.instructions.deposit.accounts,
            "strategyInitReceipt",
        )?;
        let strategy_asset_ata = labeled_account(
            &manifest.instructions.deposit.accounts,
            "vaultStrategyAssetAta",
        )?;
        let manifest_idle_authority = labeled_account(
            &manifest.instructions.deposit.accounts,
            "vaultAssetIdleAuth",
        )?;
        require_equal(
            &mut idle_authority,
            manifest_idle_authority,
            "vault idle authority",
        )?;
        require_bundle(
            labeled_account(
                &manifest.instructions.withdraw.accounts,
                "strategyInitReceipt",
            )? == strategy_init_receipt
                && labeled_account(
                    &manifest.instructions.withdraw.accounts,
                    "vaultStrategyAssetAta",
                )? == strategy_asset_ata
                && labeled_account(
                    &manifest.instructions.withdraw.accounts,
                    "vaultAssetIdleAuth",
                )? == manifest_idle_authority,
            "deposit and withdraw strategy state accounts drifted",
        )?;
        let operation_cap = maximum_raw.expect("manager cap was just populated");
        validate_runtime_template(
            manager,
            &VoltrKaminoRuntimePolicyTemplate {
                vault: manifest_vault,
                reserve,
                max_operation_amount_raw: operation_cap,
                deposit_instruction: deposit_inner.clone(),
                withdraw_instruction: withdraw_inner.clone(),
            },
        )
        .map_err(|error| {
            BackyardVoltrBundleError::Invalid(format!(
                "embedded {strategy:?} instruction graph failed Rust validation: {error}"
            ))
        })?;

        for (operation, inner, expected_seed) in [
            (
                BackyardVoltrManagerOperation::Deposit,
                deposit_inner,
                parse_u64(&manifest.policy_seeds.deposit, "deposit policy seed")?,
            ),
            (
                BackyardVoltrManagerOperation::Withdraw,
                withdraw_inner,
                parse_u64(&manifest.policy_seeds.withdraw, "withdraw policy seed")?,
            ),
        ] {
            let expected_catalog_seed = policy_seed_before
                .checked_add(1 + (manifest_index as u64) * 2)
                .and_then(|seed| {
                    (operation == BackyardVoltrManagerOperation::Withdraw)
                        .then_some(seed + 1)
                        .or(Some(seed))
                })
                .ok_or_else(|| {
                    BackyardVoltrBundleError::Invalid("policy seed overflow".to_owned())
                })?;
            require_bundle(
                expected_seed == expected_catalog_seed,
                "source manifest policy seeds are not contiguous",
            )?;
            let policy_artifact = catalog
                .policies
                .iter()
                .find(|policy| {
                    policy.strategy_id == strategy.as_str()
                        && policy.operation == operation.as_str()
                })
                .ok_or_else(|| {
                    BackyardVoltrBundleError::Invalid(format!(
                        "missing {} {} policy artifact",
                        strategy.as_str(),
                        operation.as_str()
                    ))
                })?;
            require_bundle(
                BackyardVoltrStrategy::parse(&policy_artifact.strategy_id)? == strategy
                    && BackyardVoltrManagerOperation::parse(&policy_artifact.operation)?
                        == operation,
                "policy strategy or operation drifted",
            )?;
            let policy_seed = parse_u64(&policy_artifact.seed, "policy seed")?;
            require_bundle(policy_seed == expected_seed, "policy seed drifted")?;
            let policy = parse_pubkey(&policy_artifact.policy, "policy")?;
            let canonical_manager_instruction =
                wire_instruction(&policy_artifact.manager_execution)?;
            templates.push(BackyardVoltrManagerTemplate {
                strategy,
                operation,
                reserve,
                lending_market,
                collateral_farm,
                strategy_init_receipt,
                strategy_asset_ata,
                policy_seed,
                policy,
                inner_instruction: inner,
                canonical_manager_instruction,
            });
        }
    }

    require_bundle(
        templates.len() == 8,
        "route bundle did not produce exactly eight templates",
    )?;
    let max_operation_amount_raw = maximum_raw.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle manager cap is absent".to_owned())
    })?;
    let settings = settings.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle settings are absent".to_owned())
    })?;
    let guardian = guardian.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle guardian is absent".to_owned())
    })?;
    let vault = vault.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle vault is absent".to_owned())
    })?;
    let vault_index = vault_index.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle vault index is absent".to_owned())
    })?;
    require_bundle(
        derive_squads_vault(&settings, vault_index).0 == manager,
        "route bundle manager is not the derived Squads vault PDA",
    )?;
    let packet_limit_bytes = packet_limit_bytes.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle packet limit is absent".to_owned())
    })?;
    let idle_authority = idle_authority.ok_or_else(|| {
        BackyardVoltrBundleError::Invalid("route bundle idle authority is absent".to_owned())
    })?;
    require_bundle(
        packet_limit_bytes == solana_sdk::packet::PACKET_DATA_SIZE,
        "route bundle packet limit drifted from Solana",
    )?;
    let route_bundle_sha256 = sha256_hex(
        format!(
            "{}:{}:{}:{}:{}:{}:{}:{}:{}:{}:{}:{}:{}",
            source_artifact_sha256,
            BACKYARD_VOLTR_ROUTE_ID,
            BACKYARD_VOLTR_ROUTE_SPEC_SHA256,
            BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS,
            BACKYARD_VOLTR_NORMAL_OPTIMIZATION_INTERVAL_SECONDS,
            BACKYARD_VOLTR_CONFIGURED_IDLE_SAFETY_BUFFER_RAW,
            BACKYARD_VOLTR_IDLE_ATA,
            BACKYARD_VOLTR_LP_MINT,
            BACKYARD_VOLTR_LOOKUP_TABLE,
            BACKYARD_VOLTR_LOOKUP_TABLE_AUTHORITY,
            BACKYARD_VOLTR_LOOKUP_TABLE_ORDERED_ADDRESSES_SHA256,
            manager,
            max_operation_amount_raw
        )
        .as_bytes(),
    );
    let bundle = BackyardVoltrRouteBundle {
        route_id: catalog.route_id,
        route_spec_sha256: catalog.route_spec_sha256,
        source_artifact_sha256,
        route_bundle_sha256,
        cluster: cluster.expect("validated route has four manifests"),
        genesis_hash: genesis_hash.expect("validated route has four manifests"),
        settings,
        manager,
        guardian,
        vault,
        vault_index,
        idle_authority,
        idle_ata: BACKYARD_VOLTR_IDLE_ATA,
        lp_mint: BACKYARD_VOLTR_LP_MINT,
        max_operation_amount_raw,
        withdrawal_wait_seconds: BACKYARD_VOLTR_WITHDRAWAL_WAIT_SECONDS,
        normal_optimization_interval_seconds: BACKYARD_VOLTR_NORMAL_OPTIMIZATION_INTERVAL_SECONDS,
        configured_idle_safety_buffer_raw: BACKYARD_VOLTR_CONFIGURED_IDLE_SAFETY_BUFFER_RAW,
        lookup_table: BACKYARD_VOLTR_LOOKUP_TABLE,
        lookup_table_authority: BACKYARD_VOLTR_LOOKUP_TABLE_AUTHORITY,
        lookup_table_address_count: BACKYARD_VOLTR_LOOKUP_TABLE_ADDRESS_COUNT,
        lookup_table_ordered_addresses_sha256: BACKYARD_VOLTR_LOOKUP_TABLE_ORDERED_ADDRESSES_SHA256,
        packet_limit_bytes,
        templates,
    };
    for template in &bundle.templates {
        let rebuilt = bundle.manager_instruction(
            template.strategy,
            template.operation,
            bundle.max_operation_amount_raw,
        )?;
        require_bundle(
            rebuilt == template.canonical_manager_instruction,
            "Rust manager wrapper does not match SDK-generated canonical instruction",
        )?;
    }
    require_bundle(
        bundle
            .manager_instruction(
                BackyardVoltrStrategy::Main,
                BackyardVoltrManagerOperation::Deposit,
                0,
            )
            .is_err(),
        "zero manager amount was accepted",
    )?;
    require_bundle(
        bundle
            .manager_instruction(
                BackyardVoltrStrategy::Main,
                BackyardVoltrManagerOperation::Deposit,
                bundle.max_operation_amount_raw.saturating_add(1),
            )
            .is_err(),
        "over-cap manager amount was accepted",
    )?;
    Ok(bundle)
}

fn labeled_account(
    accounts: &[EmbeddedCanonicalAccount],
    label: &str,
) -> Result<Pubkey, BackyardVoltrBundleError> {
    let account = accounts
        .iter()
        .find(|account| account.label == label)
        .ok_or_else(|| {
            BackyardVoltrBundleError::Invalid(format!("canonical instruction omitted {label}"))
        })?;
    parse_pubkey(&account.address, label)
}

fn canonical_instruction(
    instruction: &EmbeddedCanonicalInstruction,
) -> Result<Instruction, BackyardVoltrBundleError> {
    let mut accounts = Vec::with_capacity(instruction.accounts.len());
    for (expected_index, account) in instruction.accounts.iter().enumerate() {
        require_bundle(
            account.index == expected_index,
            "canonical instruction account indexes are not contiguous",
        )?;
        accounts.push(solana_sdk::instruction::AccountMeta {
            pubkey: parse_pubkey(&account.address, "canonical instruction account")?,
            is_signer: account.signer,
            is_writable: account.writable,
        });
    }
    decoded_instruction(
        &instruction.program_id,
        &instruction.data_base64,
        &instruction.data_sha256,
        instruction.data_length,
        accounts,
    )
}

fn wire_instruction(
    instruction: &EmbeddedWireInstruction,
) -> Result<Instruction, BackyardVoltrBundleError> {
    let accounts = instruction
        .accounts
        .iter()
        .map(|account| {
            Ok(solana_sdk::instruction::AccountMeta {
                pubkey: parse_pubkey(&account.address, "manager instruction account")?,
                is_signer: account.signer,
                is_writable: account.writable,
            })
        })
        .collect::<Result<Vec<_>, BackyardVoltrBundleError>>()?;
    decoded_instruction(
        &instruction.program_id,
        &instruction.data_base64,
        &instruction.data_sha256,
        instruction.data_length,
        accounts,
    )
}

fn decoded_instruction(
    program_id: &str,
    data_base64: &str,
    data_sha256: &str,
    data_length: usize,
    accounts: Vec<solana_sdk::instruction::AccountMeta>,
) -> Result<Instruction, BackyardVoltrBundleError> {
    let data = BASE64.decode(data_base64).map_err(|error| {
        BackyardVoltrBundleError::Invalid(format!(
            "canonical instruction data is invalid base64: {error}"
        ))
    })?;
    require_bundle(
        data.len() == data_length,
        "canonical instruction data length drifted",
    )?;
    require_bundle(
        sha256_hex(&data) == data_sha256,
        "canonical instruction data hash drifted",
    )?;
    Ok(Instruction {
        program_id: parse_pubkey(program_id, "instruction program")?,
        accounts,
        data,
    })
}

fn parse_pubkey(value: &str, field: &str) -> Result<Pubkey, BackyardVoltrBundleError> {
    Pubkey::from_str(value).map_err(|error| {
        BackyardVoltrBundleError::Invalid(format!("invalid {field} pubkey: {error}"))
    })
}

fn parse_u64(value: &str, field: &str) -> Result<u64, BackyardVoltrBundleError> {
    value
        .parse::<u64>()
        .map_err(|error| BackyardVoltrBundleError::Invalid(format!("invalid {field} u64: {error}")))
}

fn require_bundle(condition: bool, message: &'static str) -> Result<(), BackyardVoltrBundleError> {
    if condition {
        Ok(())
    } else {
        Err(BackyardVoltrBundleError::Invalid(message.to_owned()))
    }
}

fn require_equal<T: PartialEq + Clone>(
    slot: &mut Option<T>,
    value: T,
    field: &'static str,
) -> Result<(), BackyardVoltrBundleError> {
    if let Some(existing) = slot {
        require_bundle(existing == &value, field)?;
    } else {
        *slot = Some(value);
    }
    Ok(())
}

fn sha256_hex(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        encoded.push_str(&format!("{byte:02x}"));
    }
    encoded
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum VoltrKaminoPolicyError {
    NonSequentialPolicySeeds,
    NonSequentialCatalogSeeds,
    InvalidStrategyCatalog {
        index: usize,
        field: &'static str,
    },
    InvalidMaximumAmount,
    InvalidInstruction {
        operation: &'static str,
        field: &'static str,
    },
    GraphMismatch(&'static str),
    Squads(crate::LoyalActionError),
}

impl fmt::Display for VoltrKaminoPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NonSequentialPolicySeeds => {
                formatter.write_str("Voltr policy seeds must be one sequential Squads sequence")
            }
            Self::NonSequentialCatalogSeeds => formatter.write_str(
                "Backyard Voltr policy catalog seeds must be four contiguous deposit/withdraw pairs",
            ),
            Self::InvalidStrategyCatalog { index, field } => {
                write!(formatter, "invalid Backyard Voltr strategy catalog entry {index} {field}")
            }
            Self::InvalidMaximumAmount => {
                formatter.write_str("Voltr maximum operation amount must be positive")
            }
            Self::InvalidInstruction { operation, field } => {
                write!(formatter, "invalid Voltr {operation} {field}")
            }
            Self::GraphMismatch(field) => {
                write!(formatter, "Voltr strategy graph mismatch: {field}")
            }
            Self::Squads(error) => write!(formatter, "Squads policy encoding failed: {error}"),
        }
    }
}

impl std::error::Error for VoltrKaminoPolicyError {}

impl From<crate::LoyalActionError> for VoltrKaminoPolicyError {
    fn from(error: crate::LoyalActionError) -> Self {
        Self::Squads(error)
    }
}

#[allow(clippy::too_many_arguments)]
pub fn create_voltr_kamino_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegated_guardian: Pubkey,
    vault_index: u8,
    seeds: VoltrKaminoPolicySeeds,
    profile: VoltrKaminoConstraintProfile,
    template: VoltrKaminoPolicyTemplate,
) -> Result<VoltrKaminoPolicies, VoltrKaminoPolicyError> {
    if template.max_operation_amount_raw == 0 {
        return Err(VoltrKaminoPolicyError::InvalidMaximumAmount);
    }
    if seeds.initialize.checked_add(1) != Some(seeds.deposit)
        || seeds.deposit.checked_add(1) != Some(seeds.withdraw)
    {
        return Err(VoltrKaminoPolicyError::NonSequentialPolicySeeds);
    }

    let manager = derive_squads_vault(&settings, vault_index).0;
    validate_template(manager, &template)?;

    let initialize_indexes = constrained_indexes(
        profile,
        INITIALIZE_ACCOUNT_COUNT,
        INITIALIZE_SECURITY_CRITICAL,
    );
    let deposit_indexes =
        constrained_indexes(profile, DEPOSIT_ACCOUNT_COUNT, DEPOSIT_SECURITY_CRITICAL);
    let withdraw_indexes =
        constrained_indexes(profile, WITHDRAW_ACCOUNT_COUNT, WITHDRAW_SECURITY_CRITICAL);

    Ok(VoltrKaminoPolicies {
        manager,
        initialize: policy_plan(
            settings,
            authority,
            delegated_guardian,
            vault_index,
            seeds.initialize,
            initialize_constraint(&template.initialize_instruction, &initialize_indexes),
            initialize_indexes,
        )?,
        deposit: policy_plan(
            settings,
            authority,
            delegated_guardian,
            vault_index,
            seeds.deposit,
            manager_constraint(
                &template.deposit_instruction,
                &deposit_indexes,
                template.max_operation_amount_raw,
            ),
            deposit_indexes,
        )?,
        withdraw: policy_plan(
            settings,
            authority,
            delegated_guardian,
            vault_index,
            seeds.withdraw,
            manager_constraint(
                &template.withdraw_instruction,
                &withdraw_indexes,
                template.max_operation_amount_raw,
            ),
            withdraw_indexes,
        )?,
    })
}

/// Compiles only the two permanent manager policies used after bootstrap.
///
/// Keeping this separate from `create_voltr_kamino_policies` prevents an
/// initialization policy from accidentally entering a runtime artifact.
#[allow(clippy::too_many_arguments)]
pub fn create_voltr_kamino_runtime_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegated_guardian: Pubkey,
    vault_index: u8,
    seeds: VoltrKaminoRuntimePolicySeeds,
    profile: VoltrKaminoConstraintProfile,
    template: VoltrKaminoRuntimePolicyTemplate,
) -> Result<VoltrKaminoRuntimePolicies, VoltrKaminoPolicyError> {
    if template.max_operation_amount_raw == 0 {
        return Err(VoltrKaminoPolicyError::InvalidMaximumAmount);
    }
    if seeds.deposit.checked_add(1) != Some(seeds.withdraw) {
        return Err(VoltrKaminoPolicyError::NonSequentialPolicySeeds);
    }

    let manager = derive_squads_vault(&settings, vault_index).0;
    validate_runtime_template(manager, &template)?;

    let deposit_indexes =
        constrained_indexes(profile, DEPOSIT_ACCOUNT_COUNT, DEPOSIT_SECURITY_CRITICAL);
    let withdraw_indexes =
        constrained_indexes(profile, WITHDRAW_ACCOUNT_COUNT, WITHDRAW_SECURITY_CRITICAL);

    Ok(VoltrKaminoRuntimePolicies {
        manager,
        deposit: policy_plan(
            settings,
            authority,
            delegated_guardian,
            vault_index,
            seeds.deposit,
            manager_constraint(
                &template.deposit_instruction,
                &deposit_indexes,
                template.max_operation_amount_raw,
            ),
            deposit_indexes,
        )?,
        withdraw: policy_plan(
            settings,
            authority,
            delegated_guardian,
            vault_index,
            seeds.withdraw,
            manager_constraint(
                &template.withdraw_instruction,
                &withdraw_indexes,
                template.max_operation_amount_raw,
            ),
            withdraw_indexes,
        )?,
    })
}

/// Build the complete fixed four-market runtime policy catalog.
///
/// This is intentionally a thin composition layer over
/// [`create_voltr_kamino_runtime_policies`].  Each entry is independently
/// validated, while this function additionally requires the exact product
/// strategy order, reserve allowlist, and contiguous policy seeds beginning
/// immediately after `policy_seed_before`.  In particular, it cannot merge
/// multiple routes into one policy or accept a caller-supplied fifth market.
pub fn create_backyard_voltr_runtime_policy_catalog(
    settings: Pubkey,
    authority: Pubkey,
    delegated_guardian: Pubkey,
    vault_index: u8,
    policy_seed_before: u64,
    specs: [BackyardVoltrRuntimePolicySpec; BACKYARD_VOLTR_STRATEGY_IDS.len()],
) -> Result<BackyardVoltrRuntimePolicyCatalog, VoltrKaminoPolicyError> {
    let mut output = Vec::with_capacity(BACKYARD_VOLTR_STRATEGY_IDS.len());
    for (index, spec) in specs.into_iter().enumerate() {
        if spec.strategy_id != BACKYARD_VOLTR_STRATEGY_IDS[index] {
            return Err(VoltrKaminoPolicyError::InvalidStrategyCatalog {
                index,
                field: "strategy id",
            });
        }
        if spec.template.reserve != BACKYARD_VOLTR_STRATEGY_RESERVES[index] {
            return Err(VoltrKaminoPolicyError::InvalidStrategyCatalog {
                index,
                field: "reserve",
            });
        }
        let expected_deposit = policy_seed_before
            .checked_add(1 + (index as u64) * 2)
            .ok_or(VoltrKaminoPolicyError::NonSequentialCatalogSeeds)?;
        let expected_withdraw = expected_deposit
            .checked_add(1)
            .ok_or(VoltrKaminoPolicyError::NonSequentialCatalogSeeds)?;
        if spec.seeds.deposit != expected_deposit || spec.seeds.withdraw != expected_withdraw {
            return Err(VoltrKaminoPolicyError::NonSequentialCatalogSeeds);
        }
        output.push(create_voltr_kamino_runtime_policies(
            settings,
            authority,
            delegated_guardian,
            vault_index,
            spec.seeds,
            spec.profile,
            spec.template,
        )?);
    }
    output
        .try_into()
        .map_err(|_| VoltrKaminoPolicyError::NonSequentialCatalogSeeds)
}

#[allow(clippy::too_many_arguments)]
fn policy_plan(
    settings: Pubkey,
    authority: Pubkey,
    delegated_guardian: Pubkey,
    vault_index: u8,
    policy_seed: u64,
    constraint: SquadsInstructionConstraint,
    constrained_account_indexes: Vec<u8>,
) -> Result<VoltrKaminoPolicyPlan, VoltrKaminoPolicyError> {
    Ok(VoltrKaminoPolicyPlan {
        policy: derive_action_account(&settings, policy_seed).0,
        policy_seed,
        create_instruction: create_program_interaction_action_instruction(
            settings,
            authority,
            delegated_guardian,
            policy_seed,
            vault_index,
            vec![constraint],
        )?,
        constrained_account_indexes,
    })
}

fn constrained_indexes(
    profile: VoltrKaminoConstraintProfile,
    account_count: usize,
    critical: &[u8],
) -> Vec<u8> {
    match profile {
        VoltrKaminoConstraintProfile::ExactAllAccounts => (0..account_count as u8).collect(),
        VoltrKaminoConstraintProfile::SecurityCritical => critical.to_vec(),
    }
}

fn initialize_constraint(
    instruction: &Instruction,
    constrained_account_indexes: &[u8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: instruction.program_id,
        account_constraints: account_constraints(instruction, constrained_account_indexes),
        data_constraints: vec![data_slice_equals(0, instruction.data.clone())],
    }
}

fn manager_constraint(
    instruction: &Instruction,
    constrained_account_indexes: &[u8],
    max_operation_amount_raw: u64,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: instruction.program_id,
        account_constraints: account_constraints(instruction, constrained_account_indexes),
        data_constraints: vec![
            data_slice_equals(0, instruction.data[..8].to_vec()),
            SquadsDataConstraint {
                data_offset: 8,
                data_value: SquadsDataValue::U64Le(0),
                operator: SquadsDataOperator::GreaterThan,
            },
            SquadsDataConstraint {
                data_offset: 8,
                data_value: SquadsDataValue::U64Le(max_operation_amount_raw),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
            // Pins option tag, encoded 8-byte adaptor discriminator and the
            // `additionalArgs = null` tag. Exact length remains a local builder
            // invariant because ProgramInteraction has no length comparator.
            data_slice_equals(16, instruction.data[16..].to_vec()),
        ],
    }
}

fn account_constraints(
    instruction: &Instruction,
    constrained_account_indexes: &[u8],
) -> Vec<SquadsAccountConstraint> {
    constrained_account_indexes
        .iter()
        .map(|index| pubkey_constraint(*index, instruction.accounts[usize::from(*index)].pubkey))
        .collect()
}

fn validate_template(
    manager: Pubkey,
    template: &VoltrKaminoPolicyTemplate,
) -> Result<(), VoltrKaminoPolicyError> {
    validate_runtime_pair(
        manager,
        template.vault,
        template.reserve,
        &template.deposit_instruction,
        &template.withdraw_instruction,
    )?;

    validate_instruction(
        "initialize",
        &template.initialize_instruction,
        INITIALIZE_ACCOUNT_COUNT,
        INITIALIZE_DATA_LENGTH,
        &INITIALIZE_METAS,
    )?;
    validate_wire(
        "initialize",
        &template.initialize_instruction.data,
        VOLTR_INITIALIZE_STRATEGY_DISCRIMINATOR,
        VOLTR_KAMINO_INITIALIZE_MARKET_DISCRIMINATOR,
    )?;

    let initialize = &template.initialize_instruction;
    let deposit = &template.deposit_instruction;
    let withdraw = &template.withdraw_instruction;

    expect_account("initialize", initialize, 0, manager, "payer manager PDA")?;
    expect_account("initialize", initialize, 1, manager, "manager PDA")?;
    expect_account("initialize", initialize, 3, template.vault, "vault")?;
    for index in [4usize, 13] {
        expect_account(
            "initialize",
            initialize,
            index,
            template.reserve,
            "strategy reserve",
        )?;
    }

    expect_account(
        "initialize",
        initialize,
        8,
        VOLTR_KAMINO_ADAPTOR_PROGRAM_ID,
        "adaptor program",
    )?;
    expect_account(
        "initialize",
        initialize,
        19,
        KAMINO_LEND_PROGRAM_ID,
        "K-Lend program",
    )?;
    expect_account(
        "initialize",
        initialize,
        17,
        KAMINO_FARMS_PROGRAM_ID,
        "Farms program",
    )?;
    expect_account(
        "initialize",
        initialize,
        9,
        solana_sdk::system_program::ID,
        "system program",
    )?;
    expect_account(
        "initialize",
        initialize,
        18,
        sysvar::rent::ID,
        "rent sysvar",
    )?;

    for (field, init_index, deposit_index, withdraw_index) in [
        ("protocol", 2, 1, 1),
        ("adaptor receipt", 5, 4, 3),
        ("strategy receipt", 6, 5, 4),
        ("strategy authority", 7, 7, 8),
        ("obligation", 11, 14, 14),
        ("lending market authority", 12, 16, 16),
        ("reserve farm", 14, 24, 24),
        ("obligation farm", 15, 23, 23),
        ("lending market", 16, 15, 15),
    ] {
        expect_same_three(
            field,
            initialize,
            init_index,
            deposit,
            deposit_index,
            withdraw,
            withdraw_index,
        )?;
    }
    if initialize.accounts[10].pubkey != deposit.accounts[25].pubkey {
        return Err(VoltrKaminoPolicyError::GraphMismatch("user metadata"));
    }

    Ok(())
}

fn validate_runtime_template(
    manager: Pubkey,
    template: &VoltrKaminoRuntimePolicyTemplate,
) -> Result<(), VoltrKaminoPolicyError> {
    validate_runtime_pair(
        manager,
        template.vault,
        template.reserve,
        &template.deposit_instruction,
        &template.withdraw_instruction,
    )
}

fn validate_runtime_pair(
    manager: Pubkey,
    vault: Pubkey,
    reserve: Pubkey,
    deposit: &Instruction,
    withdraw: &Instruction,
) -> Result<(), VoltrKaminoPolicyError> {
    validate_instruction(
        "deposit",
        deposit,
        DEPOSIT_ACCOUNT_COUNT,
        MANAGER_OPERATION_DATA_LENGTH,
        &DEPOSIT_METAS,
    )?;
    validate_instruction(
        "withdraw",
        withdraw,
        WITHDRAW_ACCOUNT_COUNT,
        MANAGER_OPERATION_DATA_LENGTH,
        &WITHDRAW_METAS,
    )?;
    validate_wire(
        "deposit",
        &deposit.data,
        VOLTR_DEPOSIT_STRATEGY_DISCRIMINATOR,
        VOLTR_KAMINO_DEPOSIT_MARKET_DISCRIMINATOR,
    )?;
    validate_wire(
        "withdraw",
        &withdraw.data,
        VOLTR_WITHDRAW_STRATEGY_DISCRIMINATOR,
        VOLTR_KAMINO_WITHDRAW_MARKET_DISCRIMINATOR,
    )?;

    expect_account("deposit", deposit, 0, manager, "manager PDA")?;
    expect_account("withdraw", withdraw, 0, manager, "manager PDA")?;
    expect_account("deposit", deposit, 2, vault, "vault")?;
    expect_account("withdraw", withdraw, 2, vault, "vault")?;
    for (operation, instruction, indexes) in [
        ("deposit", deposit, &[3usize, 17][..]),
        ("withdraw", withdraw, &[5usize, 17][..]),
    ] {
        for index in indexes {
            expect_account(operation, instruction, *index, reserve, "strategy reserve")?;
        }
    }
    expect_account(
        "deposit",
        deposit,
        13,
        VOLTR_KAMINO_ADAPTOR_PROGRAM_ID,
        "adaptor program",
    )?;
    expect_account(
        "withdraw",
        withdraw,
        6,
        VOLTR_KAMINO_ADAPTOR_PROGRAM_ID,
        "adaptor program",
    )?;
    expect_account("deposit", deposit, 8, USDC_MINT, "asset mint")?;
    expect_account("withdraw", withdraw, 9, USDC_MINT, "asset mint")?;
    expect_account(
        "deposit",
        deposit,
        30,
        KAMINO_LEND_PROGRAM_ID,
        "K-Lend program",
    )?;
    expect_account(
        "withdraw",
        withdraw,
        27,
        KAMINO_LEND_PROGRAM_ID,
        "K-Lend program",
    )?;
    expect_account(
        "deposit",
        deposit,
        29,
        KAMINO_FARMS_PROGRAM_ID,
        "Farms program",
    )?;
    expect_account(
        "withdraw",
        withdraw,
        26,
        KAMINO_FARMS_PROGRAM_ID,
        "Farms program",
    )?;
    for (operation, instruction, indexes) in [
        ("deposit", deposit, &[12usize, 21][..]),
        ("withdraw", withdraw, &[13usize, 21][..]),
    ] {
        for index in indexes {
            expect_account(
                operation,
                instruction,
                *index,
                spl_token::id(),
                "classic token program",
            )?;
        }
    }
    expect_account(
        "deposit",
        deposit,
        22,
        sysvar::instructions::ID,
        "instructions sysvar",
    )?;
    expect_account(
        "withdraw",
        withdraw,
        22,
        sysvar::instructions::ID,
        "instructions sysvar",
    )?;

    for (field, deposit_index, withdraw_index) in [
        ("protocol", 1, 1),
        ("vault", 2, 2),
        ("adaptor receipt", 4, 3),
        ("strategy receipt", 5, 4),
        ("idle authority", 6, 7),
        ("strategy authority", 7, 8),
        ("asset mint", 8, 9),
        ("LP mint", 9, 10),
        ("idle asset ATA", 10, 11),
        ("strategy asset ATA", 11, 12),
        ("obligation", 14, 14),
        ("lending market", 15, 15),
        ("lending market authority", 16, 16),
        ("reserve", 17, 17),
        ("reserve liquidity supply", 18, 20),
        ("reserve collateral mint", 19, 19),
        ("reserve collateral supply", 20, 18),
        ("instruction sysvar", 22, 22),
        ("obligation farm", 23, 23),
        ("reserve farm", 24, 24),
        ("scope", 26, 25),
        ("Farms program", 29, 26),
        ("K-Lend program", 30, 27),
    ] {
        expect_same_two(field, deposit, deposit_index, withdraw, withdraw_index)?;
    }

    Ok(())
}

fn validate_instruction(
    operation: &'static str,
    instruction: &Instruction,
    account_count: usize,
    data_length: usize,
    expected_metas: &[(bool, bool)],
) -> Result<(), VoltrKaminoPolicyError> {
    if instruction.program_id != VOLTR_VAULT_PROGRAM_ID {
        return Err(invalid(operation, "program"));
    }
    if instruction.accounts.len() != account_count {
        return Err(invalid(operation, "account count"));
    }
    if instruction.data.len() != data_length {
        return Err(invalid(operation, "data length"));
    }
    if instruction
        .accounts
        .iter()
        .zip(expected_metas)
        .any(|(actual, expected)| (actual.is_signer, actual.is_writable) != *expected)
    {
        return Err(invalid(operation, "signer/writable metas"));
    }
    Ok(())
}

fn validate_wire(
    operation: &'static str,
    data: &[u8],
    outer_discriminator: [u8; 8],
    adaptor_discriminator: [u8; 8],
) -> Result<(), VoltrKaminoPolicyError> {
    let adaptor_offset = if operation == "initialize" { 13 } else { 21 };
    let option_offset = if operation == "initialize" { 8 } else { 16 };
    if data.get(..8) != Some(outer_discriminator.as_slice())
        || data.get(option_offset) != Some(&1)
        || data.get(option_offset + 1..option_offset + 5) != Some(8u32.to_le_bytes().as_slice())
        || data.get(adaptor_offset..adaptor_offset + 8) != Some(adaptor_discriminator.as_slice())
        || data.last() != Some(&0)
    {
        return Err(invalid(operation, "instruction wire"));
    }
    Ok(())
}

fn expect_account(
    operation: &'static str,
    instruction: &Instruction,
    index: usize,
    expected: Pubkey,
    field: &'static str,
) -> Result<(), VoltrKaminoPolicyError> {
    if instruction.accounts[index].pubkey != expected {
        return Err(invalid(operation, field));
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn expect_same_three(
    field: &'static str,
    first: &Instruction,
    first_index: usize,
    second: &Instruction,
    second_index: usize,
    third: &Instruction,
    third_index: usize,
) -> Result<(), VoltrKaminoPolicyError> {
    let expected = first.accounts[first_index].pubkey;
    if second.accounts[second_index].pubkey != expected
        || third.accounts[third_index].pubkey != expected
    {
        return Err(VoltrKaminoPolicyError::GraphMismatch(field));
    }
    Ok(())
}

fn expect_same_two(
    field: &'static str,
    first: &Instruction,
    first_index: usize,
    second: &Instruction,
    second_index: usize,
) -> Result<(), VoltrKaminoPolicyError> {
    if first.accounts[first_index].pubkey != second.accounts[second_index].pubkey {
        return Err(VoltrKaminoPolicyError::GraphMismatch(field));
    }
    Ok(())
}

fn invalid(operation: &'static str, field: &'static str) -> VoltrKaminoPolicyError {
    VoltrKaminoPolicyError::InvalidInstruction { operation, field }
}

fn pubkey_constraint(account_index: u8, pubkey: Pubkey) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::Pubkey(vec![pubkey]),
        owner: None,
    }
}

fn data_slice_equals(data_offset: u64, value: Vec<u8>) -> SquadsDataConstraint {
    SquadsDataConstraint {
        data_offset,
        data_value: SquadsDataValue::U8Slice(value),
        operator: SquadsDataOperator::Equals,
    }
}
