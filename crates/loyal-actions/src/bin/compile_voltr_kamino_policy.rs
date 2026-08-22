use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_actions::autonomous_vaults::{
    create_voltr_kamino_policies, VoltrKaminoConstraintProfile, VoltrKaminoPolicies,
    VoltrKaminoPolicyPlan, VoltrKaminoPolicySeeds, VoltrKaminoPolicyTemplate,
};
use loyal_actions::{
    compile_squads_inner_instruction, decode_squads_policy_create_actions,
    execute_program_interaction_policy_instruction, SquadsAccountConstraintKindView,
    SquadsDataOperatorView, SquadsDataValueView,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    address_lookup_table::AddressLookupTableAccount,
    compute_budget::ComputeBudgetInstruction,
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::Signature,
    transaction::VersionedTransaction,
};
use std::{collections::HashSet, error::Error, fs, path::PathBuf, str::FromStr};

const SQUADS_HEAP_FRAME_BYTES: u32 = 256_000;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Manifest {
    schema_version: u64,
    ids: ManifestIds,
    limits: ManifestLimits,
    policy_seeds: ManifestPolicySeeds,
    lookup_tables: ManifestLookupTables,
    instructions: ManifestInstructions,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestLookupTables {
    tables: Vec<ManifestLookupTable>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestLookupTable {
    address: String,
    authority: String,
    active: bool,
    addresses: Vec<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestIds {
    squads_settings: String,
    manager: String,
    guardian: String,
    admin: String,
    vault: String,
    reserve: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestLimits {
    max_per_operation_raw: String,
    solana_packet_bytes: usize,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestPolicySeeds {
    current_at_finalized: String,
    initialize: String,
    deposit: String,
    withdraw: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestInstructions {
    initialize_strategy: CanonicalInstruction,
    deposit_strategy: CanonicalInstruction,
    withdraw_strategy: CanonicalInstruction,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct CanonicalInstruction {
    program_id: String,
    data_hex: String,
    data_base64: String,
    data_sha256: String,
    data_length: usize,
    accounts: Vec<CanonicalAccount>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct CanonicalAccount {
    index: usize,
    address: String,
    signer: bool,
    writable: bool,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct Report {
    schema_version: u64,
    verdict: &'static str,
    broadcast: bool,
    artifact_scope: &'static str,
    source_manifest: String,
    manager: String,
    manager_is_squads_vault: bool,
    initialization_payer: String,
    initialization_payer_is_manager: bool,
    packet_limit: usize,
    current_candidate_ready: bool,
    planned_extension_feasible: bool,
    candidate_lookup_tables: Vec<CandidateLookupTableReport>,
    candidate_lookup_table_overlay: Vec<String>,
    policy_seeds: PolicySeedsReport,
    exact_all_accounts: ProfileReport,
    security_critical: ProfileReport,
    security_critical_policy_create_artifacts: Vec<InstructionArtifact>,
    manager_execution: Vec<InstructionReport>,
    manager_execution_artifacts: Vec<ManagerExecutionArtifact>,
    omitted_security_critical_support_indexes: OmittedIndexes,
    enforcement_boundary: EnforcementBoundary,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct PolicySeedsReport {
    current_at_finalized: String,
    initialize: String,
    deposit: String,
    withdraw: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct CandidateLookupTableReport {
    address: String,
    authority: String,
    active: bool,
    address_count: usize,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct InstructionArtifact {
    operation: &'static str,
    program_id: String,
    accounts: Vec<InstructionAccountArtifact>,
    data_base64: String,
    data_sha256: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct InstructionAccountArtifact {
    address: String,
    signer: bool,
    writable: bool,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ManagerExecutionArtifact {
    operation: &'static str,
    policy: String,
    constrained_account_indexes: Vec<u8>,
    program_id: String,
    accounts: Vec<InstructionAccountArtifact>,
    data_base64: String,
    data_sha256: String,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ProfileReport {
    profile: &'static str,
    policy_creates: Vec<InstructionReport>,
    all_fit_raw: bool,
    all_fit_best_case_alt: bool,
    all_fit_with_heap_frame_raw: bool,
    all_fit_with_heap_frame_best_case_alt: bool,
    all_fit_with_heap_frame_candidate_alt: bool,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct InstructionReport {
    operation: &'static str,
    policy: Option<String>,
    constrained_account_indexes: Vec<u8>,
    instruction_data_bytes: usize,
    raw: PacketMeasurement,
    best_case_alt: PacketMeasurement,
    candidate_alt: PacketMeasurement,
    with_heap_frame_raw: PacketMeasurement,
    with_heap_frame_best_case_alt: PacketMeasurement,
    with_heap_frame_candidate_alt: PacketMeasurement,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct PacketMeasurement {
    packet_bytes: usize,
    fits: bool,
    static_accounts: usize,
    lookup_accounts: usize,
    required_signatures: u8,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct OmittedIndexes {
    initialize: Vec<u8>,
    deposit: Vec<u8>,
    withdraw: Vec<u8>,
    note: &'static str,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct EnforcementBoundary {
    squads: Vec<&'static str>,
    local_manifest: Vec<&'static str>,
    deployed_programs: Vec<&'static str>,
}

fn main() -> Result<(), Box<dyn Error>> {
    let cli = parse_cli()?;
    let manifest_path = cli.manifest_path.clone();
    let raw = fs::read_to_string(&manifest_path)?;
    let manifest: Manifest = serde_json::from_str(&raw)?;
    if manifest.schema_version != 1 {
        return Err(format!("unsupported manifest schema {}", manifest.schema_version).into());
    }
    if manifest.limits.solana_packet_bytes != PACKET_DATA_SIZE {
        return Err(format!(
            "manifest packet limit {} does not match SDK {PACKET_DATA_SIZE}",
            manifest.limits.solana_packet_bytes
        )
        .into());
    }

    let settings = parse_pubkey(&manifest.ids.squads_settings, "squads settings")?;
    let manager = parse_pubkey(&manifest.ids.manager, "manager")?;
    let guardian = parse_pubkey(&manifest.ids.guardian, "guardian")?;
    let authority = parse_pubkey(&manifest.ids.admin, "admin")?;
    let vault = parse_pubkey(&manifest.ids.vault, "vault")?;
    let reserve = parse_pubkey(&manifest.ids.reserve, "reserve")?;
    let max_operation_amount_raw = manifest.limits.max_per_operation_raw.parse::<u64>()?;

    let initialize_instruction = decode_instruction(&manifest.instructions.initialize_strategy)?;
    let deposit_instruction = decode_instruction(&manifest.instructions.deposit_strategy)?;
    let withdraw_instruction = decode_instruction(&manifest.instructions.withdraw_strategy)?;
    let initialization_payer = initialize_instruction.accounts[0].pubkey;
    let mut candidate_lookup_tables = decode_lookup_tables(&manifest.lookup_tables)?;
    apply_lookup_table_overlay(&mut candidate_lookup_tables, &cli.lookup_table_overlay)?;

    let template = VoltrKaminoPolicyTemplate {
        vault,
        reserve,
        max_operation_amount_raw,
        initialize_instruction,
        deposit_instruction,
        withdraw_instruction,
    };
    let current_policy_seed = manifest.policy_seeds.current_at_finalized.parse::<u64>()?;
    let manifest_seeds = VoltrKaminoPolicySeeds {
        initialize: manifest.policy_seeds.initialize.parse::<u64>()?,
        deposit: manifest.policy_seeds.deposit.parse::<u64>()?,
        withdraw: manifest.policy_seeds.withdraw.parse::<u64>()?,
    };
    let seeds = cli.policy_seeds.unwrap_or(manifest_seeds);
    if cli.policy_seeds.is_none()
        && (seeds.initialize != current_policy_seed.saturating_add(1)
            || seeds.deposit != seeds.initialize.saturating_add(1)
            || seeds.withdraw != seeds.deposit.saturating_add(1))
    {
        return Err("manifest policy seeds are not the next sequential Squads seeds".into());
    }
    if seeds.deposit != seeds.initialize.saturating_add(1)
        || seeds.withdraw != seeds.deposit.saturating_add(1)
    {
        return Err("policy seed override is not one sequential Squads sequence".into());
    }

    let exact = create_voltr_kamino_policies(
        settings,
        authority,
        guardian,
        1,
        seeds,
        VoltrKaminoConstraintProfile::ExactAllAccounts,
        template.clone(),
    )?;
    let critical = create_voltr_kamino_policies(
        settings,
        authority,
        guardian,
        1,
        seeds,
        VoltrKaminoConstraintProfile::SecurityCritical,
        template.clone(),
    )?;
    if exact.manager != manager || critical.manager != manager {
        return Err("manifest manager is not the derived Squads vault PDA at index 1".into());
    }
    verify_serialized_policy_set(&exact, settings, authority, guardian, 1, &template)?;
    verify_serialized_policy_set(&critical, settings, authority, guardian, 1, &template)?;

    let exact_report = profile_report(
        "exact-all-accounts",
        &exact,
        authority,
        &candidate_lookup_tables,
    )?;
    let critical_report = profile_report(
        "security-critical",
        &critical,
        authority,
        &candidate_lookup_tables,
    )?;
    let (manager_execution, manager_execution_artifacts) =
        execution_reports(&critical, guardian, 1, &template, &candidate_lookup_tables)?;
    // Policy creation is a lightweight Settings mutation and mainnet simulation
    // proved it does not need a heap request. Manager execution is measured
    // against the real finalized candidate ALT; compute/heap remains a live
    // simulation gate once the dependent Voltr accounts exist.
    let critical_ready = critical_report.all_fit_raw
        && manager_execution
            .iter()
            .all(|measurement| measurement.candidate_alt.fits);
    let planned_extension_feasible = critical_report.all_fit_best_case_alt
        && manager_execution
            .iter()
            .all(|measurement| measurement.best_case_alt.fits);

    let report = Report {
        schema_version: 1,
        verdict: if critical_ready && cli.policy_seeds.is_some() {
            "RUNTIME_MANAGER_EXECUTION_ARTIFACT_READY"
        } else if critical_ready {
            "READY_FOR_SEQUENTIAL_POLICY_INSTALLATION"
        } else if planned_extension_feasible {
            "PENDING_FINALIZED_LOOKUP_TABLE_EXTENSION"
        } else {
            "BLOCKED_BY_PACKET_LIMIT"
        },
        broadcast: false,
        artifact_scope: if cli.policy_seeds.is_some() {
            "manager-execution-for-existing-policy-seeds"
        } else {
            "sequential-policy-installation"
        },
        source_manifest: manifest_path.display().to_string(),
        manager: manager.to_string(),
        manager_is_squads_vault: true,
        initialization_payer: initialization_payer.to_string(),
        initialization_payer_is_manager: initialization_payer == manager,
        packet_limit: PACKET_DATA_SIZE,
        current_candidate_ready: critical_ready,
        planned_extension_feasible,
        candidate_lookup_tables: manifest
            .lookup_tables
            .tables
            .iter()
            .map(|table| CandidateLookupTableReport {
                address: table.address.clone(),
                authority: table.authority.clone(),
                active: table.active,
                address_count: table.addresses.len(),
            })
            .collect(),
        candidate_lookup_table_overlay: cli
            .lookup_table_overlay
            .iter()
            .map(ToString::to_string)
            .collect(),
        policy_seeds: PolicySeedsReport {
            current_at_finalized: current_policy_seed.to_string(),
            initialize: seeds.initialize.to_string(),
            deposit: seeds.deposit.to_string(),
            withdraw: seeds.withdraw.to_string(),
        },
        exact_all_accounts: exact_report,
        security_critical: critical_report,
        security_critical_policy_create_artifacts: vec![
            instruction_artifact(
                "initialize",
                &critical.initialize.create_instruction,
            ),
            instruction_artifact("deposit", &critical.deposit.create_instruction),
            instruction_artifact("withdraw", &critical.withdraw.create_instruction),
        ],
        manager_execution,
        manager_execution_artifacts,
        omitted_security_critical_support_indexes: OmittedIndexes {
            initialize: omitted_indexes(INITIALIZE_ACCOUNT_COUNT, &critical.initialize),
            deposit: omitted_indexes(DEPOSIT_ACCOUNT_COUNT, &critical.deposit),
            withdraw: omitted_indexes(WITHDRAW_ACCOUNT_COUNT, &critical.withdraw),
            note: "Omitted accounts are protocol-derived support accounts. Live Voltr/Kamino simulation must prove their validation before policy installation.",
        },
        enforcement_boundary: EnforcementBoundary {
            squads: vec![
                "outer Voltr program id",
                "selected exact account pubkeys by index",
                "outer and adaptor discriminators",
                "additionalArgs null tag",
                "zero-excluded bounded u64 amount",
            ],
            local_manifest: vec![
                "exact account count and order",
                "signer and writable metas",
                "exact instruction data length and no trailing bytes",
                "full generated account graph",
            ],
            deployed_programs: vec![
                "Voltr PDA and receipt validation",
                "Kamino reserve-owned account and authority validation",
                "hidden CPI account validation",
            ],
        },
    };

    let serialized = format!("{}\n", serde_json::to_string_pretty(&report)?);
    if let Some(path) = cli.out {
        fs::write(&path, serialized)?;
        println!(
            "{}",
            serde_json::json!({
                "wrote": path,
                "verdict": report.verdict,
                "broadcast": false,
            })
        );
    } else {
        print!("{serialized}");
    }
    if !critical_ready {
        std::process::exit(2);
    }
    Ok(())
}

const INITIALIZE_ACCOUNT_COUNT: usize = 20;
const DEPOSIT_ACCOUNT_COUNT: usize = 31;
const WITHDRAW_ACCOUNT_COUNT: usize = 28;

struct Cli {
    manifest_path: PathBuf,
    lookup_table_overlay: Vec<Pubkey>,
    policy_seeds: Option<VoltrKaminoPolicySeeds>,
    out: Option<PathBuf>,
}

fn parse_cli() -> Result<Cli, Box<dyn Error>> {
    let mut manifest_path = None;
    let mut lookup_table_overlay = Vec::new();
    let mut policy_seeds = None;
    let mut out = None;
    let mut args = std::env::args().skip(1);
    while let Some(flag) = args.next() {
        let value = args
            .next()
            .ok_or_else(|| format!("missing value for {flag}"))?;
        match flag.as_str() {
            "--manifest" => manifest_path = Some(PathBuf::from(value)),
            "--lookup-table-overlay" => {
                lookup_table_overlay = value
                    .split(',')
                    .filter(|value| !value.is_empty())
                    .map(|value| parse_pubkey(value, "lookup table overlay"))
                    .collect::<Result<Vec<_>, _>>()?;
            }
            "--policy-seeds" => {
                let parsed = value
                    .split(',')
                    .map(|seed| seed.parse::<u64>())
                    .collect::<Result<Vec<_>, _>>()?;
                if parsed.len() != 3 {
                    return Err("--policy-seeds requires initialize,deposit,withdraw".into());
                }
                policy_seeds = Some(VoltrKaminoPolicySeeds {
                    initialize: parsed[0],
                    deposit: parsed[1],
                    withdraw: parsed[2],
                });
            }
            "--out" => out = Some(PathBuf::from(value)),
            _ => return Err(format!("unsupported argument {flag}").into()),
        }
    }
    Ok(Cli {
        manifest_path: manifest_path.ok_or("--manifest is required")?,
        lookup_table_overlay,
        policy_seeds,
        out,
    })
}

fn parse_pubkey(value: &str, label: &str) -> Result<Pubkey, Box<dyn Error>> {
    Pubkey::from_str(value).map_err(|error| format!("invalid {label}: {error}").into())
}

fn decode_instruction(value: &CanonicalInstruction) -> Result<Instruction, Box<dyn Error>> {
    let data = BASE64.decode(&value.data_base64)?;
    if data.len() != value.data_length {
        return Err(format!(
            "instruction data length mismatch: decoded {}, manifest {}",
            data.len(),
            value.data_length
        )
        .into());
    }
    if lower_hex(&data) != value.data_hex {
        return Err("instruction dataHex/dataBase64 mismatch".into());
    }
    let digest = Sha256::digest(&data);
    if lower_hex(&digest) != value.data_sha256 {
        return Err("instruction data SHA-256 mismatch".into());
    }

    let mut accounts = Vec::with_capacity(value.accounts.len());
    for (expected_index, account) in value.accounts.iter().enumerate() {
        if account.index != expected_index {
            return Err(format!(
                "non-canonical account index {} at position {expected_index}",
                account.index
            )
            .into());
        }
        let pubkey = parse_pubkey(&account.address, "instruction account")?;
        accounts.push(AccountMeta {
            pubkey,
            is_signer: account.signer,
            is_writable: account.writable,
        });
    }
    Ok(Instruction {
        program_id: parse_pubkey(&value.program_id, "instruction program")?,
        accounts,
        data,
    })
}

fn decode_lookup_tables(
    lookup_tables: &ManifestLookupTables,
) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
    lookup_tables
        .tables
        .iter()
        .filter(|table| table.active)
        .map(|table| {
            Ok(AddressLookupTableAccount {
                key: parse_pubkey(&table.address, "lookup table")?,
                addresses: table
                    .addresses
                    .iter()
                    .map(|value| parse_pubkey(value, "lookup table entry"))
                    .collect::<Result<Vec<_>, _>>()?,
            })
        })
        .collect()
}

fn apply_lookup_table_overlay(
    lookup_tables: &mut [AddressLookupTableAccount],
    overlay: &[Pubkey],
) -> Result<(), Box<dyn Error>> {
    if overlay.is_empty() {
        return Ok(());
    }
    let table = lookup_tables
        .first_mut()
        .ok_or("lookup table overlay requires one active candidate table")?;
    for address in overlay {
        if !table.addresses.contains(address) {
            table.addresses.push(*address);
        }
    }
    if table.addresses.len() > 256 {
        return Err("lookup table overlay exceeds 256 addresses".into());
    }
    Ok(())
}

fn lower_hex(bytes: &[u8]) -> String {
    let mut result = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        use std::fmt::Write as _;
        write!(&mut result, "{byte:02x}").expect("writing into String cannot fail");
    }
    result
}

fn instruction_artifact(operation: &'static str, instruction: &Instruction) -> InstructionArtifact {
    InstructionArtifact {
        operation,
        program_id: instruction.program_id.to_string(),
        accounts: instruction
            .accounts
            .iter()
            .map(|account| InstructionAccountArtifact {
                address: account.pubkey.to_string(),
                signer: account.is_signer,
                writable: account.is_writable,
            })
            .collect(),
        data_base64: BASE64.encode(&instruction.data),
        data_sha256: lower_hex(&Sha256::digest(&instruction.data)),
    }
}

fn manager_execution_artifact(
    operation: &'static str,
    policy: &VoltrKaminoPolicyPlan,
    instruction: &Instruction,
) -> ManagerExecutionArtifact {
    let artifact = instruction_artifact(operation, instruction);
    ManagerExecutionArtifact {
        operation: artifact.operation,
        policy: policy.policy.to_string(),
        constrained_account_indexes: policy.constrained_account_indexes.clone(),
        program_id: artifact.program_id,
        accounts: artifact.accounts,
        data_base64: artifact.data_base64,
        data_sha256: artifact.data_sha256,
    }
}

fn profile_report(
    profile: &'static str,
    policies: &VoltrKaminoPolicies,
    payer: Pubkey,
    candidate_lookup_tables: &[AddressLookupTableAccount],
) -> Result<ProfileReport, Box<dyn Error>> {
    let policy_creates = vec![
        instruction_report(
            "initialize",
            Some(&policies.initialize),
            &policies.initialize.create_instruction,
            payer,
            candidate_lookup_tables,
        )?,
        instruction_report(
            "deposit",
            Some(&policies.deposit),
            &policies.deposit.create_instruction,
            payer,
            candidate_lookup_tables,
        )?,
        instruction_report(
            "withdraw",
            Some(&policies.withdraw),
            &policies.withdraw.create_instruction,
            payer,
            candidate_lookup_tables,
        )?,
    ];
    Ok(ProfileReport {
        profile,
        all_fit_raw: policy_creates
            .iter()
            .all(|measurement| measurement.raw.fits),
        all_fit_best_case_alt: policy_creates
            .iter()
            .all(|measurement| measurement.best_case_alt.fits),
        all_fit_with_heap_frame_raw: policy_creates
            .iter()
            .all(|measurement| measurement.with_heap_frame_raw.fits),
        all_fit_with_heap_frame_best_case_alt: policy_creates
            .iter()
            .all(|measurement| measurement.with_heap_frame_best_case_alt.fits),
        all_fit_with_heap_frame_candidate_alt: policy_creates
            .iter()
            .all(|measurement| measurement.with_heap_frame_candidate_alt.fits),
        policy_creates,
    })
}

fn execution_reports(
    policies: &VoltrKaminoPolicies,
    guardian: Pubkey,
    vault_index: u8,
    template: &VoltrKaminoPolicyTemplate,
    candidate_lookup_tables: &[AddressLookupTableAccount],
) -> Result<(Vec<InstructionReport>, Vec<ManagerExecutionArtifact>), Box<dyn Error>> {
    let mut reports = Vec::new();
    let mut artifacts = Vec::new();
    for (operation, policy, inner) in [
        (
            "initialize",
            &policies.initialize,
            &template.initialize_instruction,
        ),
        ("deposit", &policies.deposit, &template.deposit_instruction),
        (
            "withdraw",
            &policies.withdraw,
            &template.withdraw_instruction,
        ),
    ] {
        let mut transaction_accounts = Vec::new();
        let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner.clone());
        let wrapped = execute_program_interaction_policy_instruction(
            policy.policy,
            guardian,
            vault_index,
            vec![compiled],
            vec![0],
            transaction_accounts,
        );
        artifacts.push(manager_execution_artifact(operation, policy, &wrapped));
        reports.push(instruction_report(
            operation,
            Some(policy),
            &wrapped,
            guardian,
            candidate_lookup_tables,
        )?);
    }
    Ok((reports, artifacts))
}

fn verify_serialized_policy_set(
    policies: &VoltrKaminoPolicies,
    settings: Pubkey,
    authority: Pubkey,
    guardian: Pubkey,
    vault_index: u8,
    template: &VoltrKaminoPolicyTemplate,
) -> Result<(), Box<dyn Error>> {
    verify_serialized_policy(
        "initialize",
        &policies.initialize,
        &template.initialize_instruction,
        settings,
        authority,
        guardian,
        vault_index,
        None,
    )?;
    verify_serialized_policy(
        "deposit",
        &policies.deposit,
        &template.deposit_instruction,
        settings,
        authority,
        guardian,
        vault_index,
        Some(template.max_operation_amount_raw),
    )?;
    verify_serialized_policy(
        "withdraw",
        &policies.withdraw,
        &template.withdraw_instruction,
        settings,
        authority,
        guardian,
        vault_index,
        Some(template.max_operation_amount_raw),
    )?;
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn verify_serialized_policy(
    operation: &'static str,
    plan: &VoltrKaminoPolicyPlan,
    inner: &Instruction,
    settings: Pubkey,
    authority: Pubkey,
    guardian: Pubkey,
    vault_index: u8,
    maximum_amount: Option<u64>,
) -> Result<(), Box<dyn Error>> {
    let actions = decode_squads_policy_create_actions(&plan.create_instruction)?;
    let [action] = actions.as_slice() else {
        return Err(format!("{operation} policy did not decode as exactly one action").into());
    };
    if action.settings != settings
        || action.authority != authority
        || action.policy_seed != plan.policy_seed
        || action.policy_account != plan.policy
        || action.delegated_signers != vec![guardian]
        || action.threshold != 1
        || action.payload.vault_index != vault_index
        || !action.payload.pubkey_table.is_empty()
        || !action.payload.spending_limits.is_empty()
        || action.payload.constraints.len() != 1
    {
        return Err(format!("{operation} decoded policy header mismatch").into());
    }
    let constraint = &action.payload.constraints[0];
    if constraint.program_id != inner.program_id
        || constraint.account_constraints.len() != plan.constrained_account_indexes.len()
    {
        return Err(format!("{operation} decoded policy constraint header mismatch").into());
    }
    for (constraint, expected_index) in constraint
        .account_constraints
        .iter()
        .zip(&plan.constrained_account_indexes)
    {
        if constraint.account_index != *expected_index
            || constraint.owner.is_some()
            || constraint.kind
                != SquadsAccountConstraintKindView::Pubkey(vec![
                    inner.accounts[usize::from(*expected_index)].pubkey,
                ])
        {
            return Err(format!(
                "{operation} decoded account constraint mismatch at {expected_index}"
            )
            .into());
        }
    }

    match maximum_amount {
        None => {
            if constraint.data_constraints.len() != 1
                || constraint.data_constraints[0].data_offset != 0
                || constraint.data_constraints[0].operator != SquadsDataOperatorView::Equals
                || constraint.data_constraints[0].data_value
                    != SquadsDataValueView::U8Slice(inner.data.clone())
            {
                return Err(format!("{operation} decoded exact data constraint mismatch").into());
            }
        }
        Some(maximum_amount) => {
            let data = &constraint.data_constraints;
            if data.len() != 4
                || data[0].data_offset != 0
                || data[0].operator != SquadsDataOperatorView::Equals
                || data[0].data_value != SquadsDataValueView::U8Slice(inner.data[..8].to_vec())
                || data[1].data_offset != 8
                || data[1].operator != SquadsDataOperatorView::GreaterThan
                || data[1].data_value != SquadsDataValueView::U64Le(0)
                || data[2].data_offset != 8
                || data[2].operator != SquadsDataOperatorView::LessThanOrEqualTo
                || data[2].data_value != SquadsDataValueView::U64Le(maximum_amount)
                || data[3].data_offset != 16
                || data[3].operator != SquadsDataOperatorView::Equals
                || data[3].data_value != SquadsDataValueView::U8Slice(inner.data[16..].to_vec())
            {
                return Err(
                    format!("{operation} decoded bounded data constraints mismatch").into(),
                );
            }
        }
    }
    Ok(())
}

fn instruction_report(
    operation: &'static str,
    policy: Option<&VoltrKaminoPolicyPlan>,
    instruction: &Instruction,
    payer: Pubkey,
    candidate_lookup_tables: &[AddressLookupTableAccount],
) -> Result<InstructionReport, Box<dyn Error>> {
    let raw = packet_measurement(std::slice::from_ref(instruction), payer, &[])?;
    let best_case_table = best_case_lookup_table(std::slice::from_ref(instruction), payer);
    let best_case_alt = packet_measurement(
        std::slice::from_ref(instruction),
        payer,
        std::slice::from_ref(&best_case_table),
    )?;
    let candidate_alt = packet_measurement(
        std::slice::from_ref(instruction),
        payer,
        candidate_lookup_tables,
    )?;
    let instructions_with_heap_frame = [
        ComputeBudgetInstruction::request_heap_frame(SQUADS_HEAP_FRAME_BYTES),
        instruction.clone(),
    ];
    let with_heap_frame_raw = packet_measurement(&instructions_with_heap_frame, payer, &[])?;
    let heap_best_case_table = best_case_lookup_table(&instructions_with_heap_frame, payer);
    let with_heap_frame_best_case_alt = packet_measurement(
        &instructions_with_heap_frame,
        payer,
        std::slice::from_ref(&heap_best_case_table),
    )?;
    let with_heap_frame_candidate_alt = packet_measurement(
        &instructions_with_heap_frame,
        payer,
        candidate_lookup_tables,
    )?;
    Ok(InstructionReport {
        operation,
        policy: policy.map(|value| value.policy.to_string()),
        constrained_account_indexes: policy
            .map(|value| value.constrained_account_indexes.clone())
            .unwrap_or_default(),
        instruction_data_bytes: instruction.data.len(),
        raw,
        best_case_alt,
        candidate_alt,
        with_heap_frame_raw,
        with_heap_frame_best_case_alt,
        with_heap_frame_candidate_alt,
    })
}

fn packet_measurement(
    instructions: &[Instruction],
    payer: Pubkey,
    lookup_tables: &[AddressLookupTableAccount],
) -> Result<PacketMeasurement, Box<dyn Error>> {
    let message =
        v0::Message::try_compile(&payer, instructions, lookup_tables, Hash::new_unique())?;
    let lookup_accounts = message
        .address_table_lookups
        .iter()
        .map(|lookup| lookup.writable_indexes.len() + lookup.readonly_indexes.len())
        .sum();
    let transaction = VersionedTransaction {
        signatures: vec![Signature::default(); usize::from(message.header.num_required_signatures)],
        message: VersionedMessage::V0(message.clone()),
    };
    let packet_bytes = bincode::serialize(&transaction)?.len();
    Ok(PacketMeasurement {
        packet_bytes,
        fits: packet_bytes <= PACKET_DATA_SIZE,
        static_accounts: message.account_keys.len(),
        lookup_accounts,
        required_signatures: message.header.num_required_signatures,
    })
}

fn best_case_lookup_table(
    instructions: &[Instruction],
    payer: Pubkey,
) -> AddressLookupTableAccount {
    let mut seen = HashSet::new();
    let addresses = instructions
        .iter()
        .flat_map(|instruction| instruction.accounts.iter())
        .filter(|account| !account.is_signer && account.pubkey != payer)
        .filter_map(|account| seen.insert(account.pubkey).then_some(account.pubkey))
        .collect();
    AddressLookupTableAccount {
        key: Pubkey::new_from_array([0x42; 32]),
        addresses,
    }
}

fn omitted_indexes(account_count: usize, policy: &VoltrKaminoPolicyPlan) -> Vec<u8> {
    (0..account_count as u8)
        .filter(|index| !policy.constrained_account_indexes.contains(index))
        .collect()
}
