use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_actions::autonomous_vaults::{
    create_voltr_kamino_runtime_policies, VoltrKaminoConstraintProfile, VoltrKaminoPolicyPlan,
    VoltrKaminoRuntimePolicies, VoltrKaminoRuntimePolicySeeds, VoltrKaminoRuntimePolicyTemplate,
};
use loyal_actions::{
    compile_squads_inner_instruction, decode_squads_policy_create_actions,
    execute_program_interaction_policy_instruction, SquadsAccountConstraintKindView,
    SquadsDataOperatorView, SquadsDataValueView,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::Signature,
    transaction::VersionedTransaction,
};
use std::{error::Error, fs, io::Read, path::PathBuf, str::FromStr};

const APPROVED_ROUTE_ID: &str = "loyal-backyard-main-usdc-v1";
const APPROVED_ROUTE_SPEC_SHA256: &str =
    "31e2de6705ccaa64df4625bc747c4fb9a6f9ff3142fd05b1132aa0ca2d90d234";
const APPROVED_FOUR_MARKET_ROUTE_ID: &str = "loyal-backyard-four-market-usdc-v1";
const APPROVED_FOUR_MARKET_ROUTE_SPEC_SHA256: &str =
    "df6547aeaba99f6bf32a0f56d63c50d30f84d7dc1d3df801266b97bd9811e8f4";
const MAINNET_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const APPROVED_SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const APPROVED_MANAGER: &str = "DMPn3d7G2rcVVhvRbpSyEeq3cBW7bygiGjSgrLci5FYK";
const APPROVED_GUARDIAN: &str = "oz8skK9o2N5w85rrkMfBVdeg6wnjAqMzriVSupERo3C";
const APPROVED_ADMIN: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const APPROVED_VAULT: &str = "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK";
const APPROVED_RESERVE: &str = "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59";
const APPROVED_LENDING_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const APPROVED_COLLATERAL_FARM: &str = "JAvnB9AKtgPsTEoKmn24Bq64UMoYcrtWtq42HHBdsPkh";
const APPROVED_VAULT_INDEX: u8 = 1;
const APPROVED_CURRENT_POLICY_SEED: u64 = 42;
const APPROVED_DEPOSIT_POLICY_SEED: u64 = 43;
const APPROVED_WITHDRAW_POLICY_SEED: u64 = 44;
const APPROVED_MAX_OPERATION_RAW: u64 = 200_000_000_000;
const TRAILING_DATA_CONSTRAINT_LIMITATION: &str = "Squads ProgramInteraction has no instruction-data length comparator: the policy pins the canonical bytes through offset 29 but cannot itself reject appended trailing bytes; exact 30-byte length remains a local canonical-builder and pre-send-verifier invariant";
const APPROVED_FOUR_MARKET_STRATEGIES: [(&str, &str, &str, &str); 4] = [
    (
        "main",
        "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
        "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF",
        "JAvnB9AKtgPsTEoKmn24Bq64UMoYcrtWtq42HHBdsPkh",
    ),
    (
        "onre",
        "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
        "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
        "GNcywqL6AZajsyyitxGQUvbihPgAzGZUqKfjYcvTj2pi",
    ),
    (
        "prime",
        "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
        "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
        "HqEqwkTmqCAVEQQaEBuSSGD2EAvcorFogqhZz46TYJyz",
    ),
    (
        "maple",
        "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
        "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
        "6Y9fzrWzGZaxdAJ2eWRg9UZpL3kqPDiVXAb67KJpWdUg",
    ),
];

const DEPOSIT_LABELS: [&str; 31] = [
    "manager",
    "protocol",
    "vault",
    "strategy",
    "adaptorAddReceipt",
    "strategyInitReceipt",
    "vaultAssetIdleAuth",
    "vaultStrategyAuth",
    "vaultAssetMint",
    "vaultLpMint",
    "vaultAssetIdleAta",
    "vaultStrategyAssetAta",
    "assetTokenProgram",
    "adaptorProgram",
    "kaminoObligation",
    "lendingMarket",
    "lendingMarketAuthority",
    "reserve",
    "reserveLiquiditySupply",
    "reserveCollateralMint",
    "reserveCollateralSupplyVault",
    "tokenProgram",
    "instructionsSysvar",
    "obligationFarm",
    "reserveFarmState",
    "userMetadata",
    "scope",
    "rentSysvar",
    "systemProgram",
    "farmsProgram",
    "klendProgram",
];

const WITHDRAW_LABELS: [&str; 28] = [
    "manager",
    "protocol",
    "vault",
    "adaptorAddReceipt",
    "strategyInitReceipt",
    "strategy",
    "adaptorProgram",
    "vaultAssetIdleAuth",
    "vaultStrategyAuth",
    "vaultAssetMint",
    "vaultLpMint",
    "vaultAssetIdleAta",
    "vaultStrategyAssetAta",
    "assetTokenProgram",
    "kaminoObligation",
    "lendingMarket",
    "lendingMarketAuthority",
    "reserve",
    "reserveCollateralSupplyVault",
    "reserveCollateralMint",
    "reserveLiquiditySupply",
    "tokenProgram",
    "instructionsSysvar",
    "obligationFarm",
    "reserveFarmState",
    "scope",
    "farmsProgram",
    "klendProgram",
];

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct RuntimeManifest {
    schema_version: u64,
    route_id: String,
    route_spec_sha256: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    strategy_id: Option<String>,
    cluster: String,
    genesis_hash: String,
    ids: ManifestIds,
    vault_index: u8,
    limits: ManifestLimits,
    policy_seeds: ManifestPolicySeeds,
    instructions: ManifestInstructions,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ManifestIds {
    squads_settings: String,
    manager: String,
    guardian: String,
    admin: String,
    vault: String,
    reserve: String,
    lending_market: String,
    collateral_farm: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ManifestLimits {
    max_per_operation_raw: String,
    solana_packet_bytes: usize,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ManifestPolicySeeds {
    policy_seed_before: String,
    deposit: String,
    withdraw: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ManifestInstructions {
    deposit: CanonicalInstruction,
    withdraw: CanonicalInstruction,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct CanonicalInstruction {
    program_id: String,
    data_hex: String,
    data_base64: String,
    data_sha256: String,
    data_length: usize,
    accounts: Vec<CanonicalAccount>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct CanonicalAccount {
    index: usize,
    label: String,
    address: String,
    signer: bool,
    writable: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Artifact {
    #[serde(flatten)]
    body: ArtifactBody,
    artifact_sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct ArtifactBody {
    schema_version: u64,
    evidence_type: String,
    verdict: String,
    broadcast: bool,
    route_id: String,
    route_spec_sha256: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    strategy_id: Option<String>,
    source_manifest_sha256: String,
    runtime_policy_count: usize,
    setup_policy_included: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    trailing_data_constraint_limitation: Option<String>,
    manager: String,
    policy_seed_before: String,
    policies: Vec<PolicyArtifact>,
    source_manifest: RuntimeManifest,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    source_manifests: Option<Vec<RuntimeManifest>>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct PolicyArtifact {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    strategy_id: Option<String>,
    operation: String,
    seed: String,
    policy: String,
    constrained_account_indexes: Vec<u8>,
    inner_instruction_data_sha256: String,
    policy_create: InstructionArtifact,
    policy_create_packet_bytes: usize,
    manager_execution: InstructionArtifact,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct InstructionArtifact {
    program_id: String,
    accounts: Vec<InstructionAccountArtifact>,
    data_length: usize,
    data_base64: String,
    data_sha256: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct InstructionAccountArtifact {
    address: String,
    signer: bool,
    writable: bool,
}

struct Cli {
    manifests: Vec<PathBuf>,
    verify_artifact: Option<PathBuf>,
    out: Option<PathBuf>,
}

fn main() -> Result<(), Box<dyn Error>> {
    let cli = parse_cli()?;
    if let Some(path) = cli.verify_artifact {
        let artifact: Artifact = serde_json::from_slice(&read_bytes(&path)?)?;
        let expected = match artifact.body.source_manifests.clone() {
            Some(manifests) => compile_manifest_catalog(manifests)?,
            None => compile_manifest(artifact.body.source_manifest.clone())?,
        };
        let expected_hash = artifact_hash(&expected)?;
        if artifact.artifact_sha256 != expected_hash
            || serde_json::to_vec(&artifact.body)? != serde_json::to_vec(&expected)?
        {
            return Err("runtime policy artifact differs from canonical recompilation".into());
        }
        println!(
            "{}",
            serde_json::to_string_pretty(&serde_json::json!({
                "verdict": "RUNTIME_POLICY_ARTIFACT_VERIFIED",
                "broadcast": false,
                "routeSpecSha256": expected.route_spec_sha256,
                "artifactSha256": expected_hash,
                "runtimePolicyCount": expected.runtime_policy_count,
                "setupPolicyIncluded": expected.setup_policy_included,
            }))?
        );
        return Ok(());
    }

    if cli.manifests.is_empty() {
        return Err("--manifest is required".into());
    }
    let manifests = cli
        .manifests
        .iter()
        .map(|path| read_manifest(path))
        .collect::<Result<Vec<_>, _>>()?;
    let body = if manifests.len() == 1 {
        compile_manifest(manifests.into_iter().next().expect("one manifest"))?
    } else {
        compile_manifest_catalog(manifests)?
    };
    let artifact = Artifact {
        artifact_sha256: artifact_hash(&body)?,
        body,
    };
    let serialized = format!("{}\n", serde_json::to_string_pretty(&artifact)?);
    if let Some(path) = cli.out {
        fs::write(&path, serialized)?;
        println!(
            "{}",
            serde_json::json!({
                "wrote": path,
                "verdict": artifact.body.verdict,
                "broadcast": false,
                "artifactSha256": artifact.artifact_sha256,
            })
        );
    } else {
        print!("{serialized}");
    }
    Ok(())
}

fn compile_manifest(manifest: RuntimeManifest) -> Result<ArtifactBody, Box<dyn Error>> {
    validate_manifest_boundary(&manifest)?;
    let settings = parse_pubkey(&manifest.ids.squads_settings, "Squads Settings")?;
    let authority = parse_pubkey(&manifest.ids.admin, "admin")?;
    let guardian = parse_pubkey(&manifest.ids.guardian, "guardian")?;
    let manager = parse_pubkey(&manifest.ids.manager, "manager")?;
    let vault = parse_pubkey(&manifest.ids.vault, "vault")?;
    let reserve = parse_pubkey(&manifest.ids.reserve, "reserve")?;
    let max_operation_amount_raw = manifest.limits.max_per_operation_raw.parse::<u64>()?;
    let deposit_seed = manifest.policy_seeds.deposit.parse::<u64>()?;
    let withdraw_seed = manifest.policy_seeds.withdraw.parse::<u64>()?;
    let deposit = decode_instruction(&manifest.instructions.deposit, &DEPOSIT_LABELS)?;
    let withdraw = decode_instruction(&manifest.instructions.withdraw, &WITHDRAW_LABELS)?;

    require_account(
        &deposit,
        15,
        &manifest.ids.lending_market,
        "deposit lending market",
    )?;
    require_account(
        &withdraw,
        15,
        &manifest.ids.lending_market,
        "withdraw lending market",
    )?;
    require_account(
        &deposit,
        24,
        &manifest.ids.collateral_farm,
        "deposit collateral farm",
    )?;
    require_account(
        &withdraw,
        24,
        &manifest.ids.collateral_farm,
        "withdraw collateral farm",
    )?;

    let template = VoltrKaminoRuntimePolicyTemplate {
        vault,
        reserve,
        max_operation_amount_raw,
        deposit_instruction: deposit.clone(),
        withdraw_instruction: withdraw.clone(),
    };
    let policies = create_voltr_kamino_runtime_policies(
        settings,
        authority,
        guardian,
        manifest.vault_index,
        VoltrKaminoRuntimePolicySeeds {
            deposit: deposit_seed,
            withdraw: withdraw_seed,
        },
        VoltrKaminoConstraintProfile::SecurityCritical,
        template,
    )?;
    if policies.manager != manager {
        return Err("manifest manager is not the derived Squads vault PDA".into());
    }
    verify_policy_pair(
        &policies,
        settings,
        authority,
        guardian,
        manifest.vault_index,
        max_operation_amount_raw,
        &deposit,
        &withdraw,
    )?;

    let policy_artifacts = [
        policy_artifact(
            "deposit",
            &policies.deposit,
            &deposit,
            guardian,
            manifest.vault_index,
            manifest.strategy_id.as_deref(),
        )?,
        policy_artifact(
            "withdraw",
            &policies.withdraw,
            &withdraw,
            guardian,
            manifest.vault_index,
            manifest.strategy_id.as_deref(),
        )?,
    ];
    if policy_artifacts
        .iter()
        .any(|artifact| artifact.policy_create_packet_bytes > PACKET_DATA_SIZE)
    {
        return Err("runtime policy-create transaction exceeds Solana packet limit".into());
    }
    let source_manifest_sha256 = digest_json(&manifest)?;
    Ok(ArtifactBody {
        schema_version: 1,
        evidence_type: "backyard-voltr-runtime-policy-artifact".into(),
        verdict: "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED".into(),
        broadcast: false,
        route_id: manifest.route_id.clone(),
        route_spec_sha256: manifest.route_spec_sha256.clone(),
        strategy_id: manifest.strategy_id.clone(),
        source_manifest_sha256,
        runtime_policy_count: 2,
        setup_policy_included: false,
        trailing_data_constraint_limitation: None,
        manager: policies.manager.to_string(),
        policy_seed_before: manifest.policy_seeds.policy_seed_before.clone(),
        policies: policy_artifacts.into(),
        source_manifest: manifest,
        source_manifests: None,
    })
}

fn compile_manifest_catalog(
    manifests: Vec<RuntimeManifest>,
) -> Result<ArtifactBody, Box<dyn Error>> {
    if manifests.len() != APPROVED_FOUR_MARKET_STRATEGIES.len() {
        return Err("four-market policy catalog requires exactly four manifests".into());
    }
    let bodies = manifests
        .iter()
        .cloned()
        .map(compile_manifest)
        .collect::<Result<Vec<_>, _>>()?;
    if bodies.iter().any(|body| {
        body.route_id != APPROVED_FOUR_MARKET_ROUTE_ID
            || body.route_spec_sha256 != APPROVED_FOUR_MARKET_ROUTE_SPEC_SHA256
            || body.strategy_id.is_none()
            || body.policies.len() != 2
    }) {
        return Err("four-market policy catalog contains a non-four-market manifest".into());
    }
    let strategy_ids = bodies
        .iter()
        .map(|body| body.strategy_id.as_deref())
        .collect::<Vec<_>>();
    let expected_strategy_ids = APPROVED_FOUR_MARKET_STRATEGIES
        .iter()
        .map(|(id, _, _, _)| Some(*id))
        .collect::<Vec<_>>();
    if strategy_ids != expected_strategy_ids {
        return Err("four-market policy catalog strategy order is not canonical".into());
    }
    let source_manifest = manifests.first().cloned().expect("catalog length checked");
    let policies = bodies
        .into_iter()
        .flat_map(|body| body.policies)
        .collect::<Vec<_>>();
    if policies.len() != 8 {
        return Err("four-market policy catalog did not produce exactly eight policies".into());
    }
    Ok(ArtifactBody {
        schema_version: 1,
        evidence_type: "backyard-voltr-runtime-policy-artifact".into(),
        verdict: "RUNTIME_POLICY_ARTIFACT_COMPILED_AND_VERIFIED".into(),
        broadcast: false,
        route_id: APPROVED_FOUR_MARKET_ROUTE_ID.into(),
        route_spec_sha256: APPROVED_FOUR_MARKET_ROUTE_SPEC_SHA256.into(),
        strategy_id: None,
        source_manifest_sha256: digest_json(&manifests)?,
        runtime_policy_count: policies.len(),
        setup_policy_included: false,
        trailing_data_constraint_limitation: Some(TRAILING_DATA_CONSTRAINT_LIMITATION.into()),
        manager: APPROVED_MANAGER.into(),
        policy_seed_before: APPROVED_CURRENT_POLICY_SEED.to_string(),
        policies,
        source_manifest,
        source_manifests: Some(manifests),
    })
}

fn validate_manifest_boundary(manifest: &RuntimeManifest) -> Result<(), Box<dyn Error>> {
    let common = manifest.schema_version == 1
        && manifest.cluster == "mainnet-beta"
        && manifest.genesis_hash == MAINNET_GENESIS_HASH
        && manifest.ids.squads_settings == APPROVED_SETTINGS
        && manifest.ids.manager == APPROVED_MANAGER
        && manifest.ids.guardian == APPROVED_GUARDIAN
        && manifest.ids.admin == APPROVED_ADMIN
        && manifest.ids.vault == APPROVED_VAULT
        && manifest.vault_index == APPROVED_VAULT_INDEX
        && manifest.limits.max_per_operation_raw == APPROVED_MAX_OPERATION_RAW.to_string()
        && manifest.limits.solana_packet_bytes == PACKET_DATA_SIZE
        && manifest.policy_seeds.policy_seed_before == APPROVED_CURRENT_POLICY_SEED.to_string();
    let main = manifest.route_id == APPROVED_ROUTE_ID
        && manifest.route_spec_sha256 == APPROVED_ROUTE_SPEC_SHA256
        && manifest.strategy_id.is_none()
        && manifest.ids.reserve == APPROVED_RESERVE
        && manifest.ids.lending_market == APPROVED_LENDING_MARKET
        && manifest.ids.collateral_farm == APPROVED_COLLATERAL_FARM
        && manifest.policy_seeds.deposit == APPROVED_DEPOSIT_POLICY_SEED.to_string()
        && manifest.policy_seeds.withdraw == APPROVED_WITHDRAW_POLICY_SEED.to_string();
    let four_market = if manifest.route_id == APPROVED_FOUR_MARKET_ROUTE_ID
        && manifest.route_spec_sha256 == APPROVED_FOUR_MARKET_ROUTE_SPEC_SHA256
    {
        let Some(strategy_id) = manifest.strategy_id.as_deref() else {
            return Err("four-market runtime manifest requires strategyId".into());
        };
        let Some((strategy_index, (_, reserve, lending_market, collateral_farm))) =
            APPROVED_FOUR_MARKET_STRATEGIES
                .iter()
                .enumerate()
                .find(|(_, (id, _, _, _))| *id == strategy_id)
        else {
            return Err("four-market runtime manifest has an unsupported strategyId".into());
        };
        let expected_deposit = APPROVED_CURRENT_POLICY_SEED + 1 + (strategy_index as u64) * 2;
        let expected_withdraw = expected_deposit + 1;
        manifest.ids.reserve == *reserve
            && manifest.ids.lending_market == *lending_market
            && manifest.ids.collateral_farm == *collateral_farm
            && manifest.policy_seeds.deposit == expected_deposit.to_string()
            && manifest.policy_seeds.withdraw == expected_withdraw.to_string()
    } else {
        false
    };
    if !common || (!main && !four_market) {
        return Err("runtime policy manifest is not the exact approved partner route".into());
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn verify_policy_pair(
    policies: &VoltrKaminoRuntimePolicies,
    settings: Pubkey,
    authority: Pubkey,
    guardian: Pubkey,
    vault_index: u8,
    maximum_amount: u64,
    deposit: &Instruction,
    withdraw: &Instruction,
) -> Result<(), Box<dyn Error>> {
    verify_policy(
        "deposit",
        &policies.deposit,
        deposit,
        settings,
        authority,
        guardian,
        vault_index,
        maximum_amount,
    )?;
    verify_policy(
        "withdraw",
        &policies.withdraw,
        withdraw,
        settings,
        authority,
        guardian,
        vault_index,
        maximum_amount,
    )
}

#[allow(clippy::too_many_arguments)]
fn verify_policy(
    operation: &str,
    plan: &VoltrKaminoPolicyPlan,
    inner: &Instruction,
    settings: Pubkey,
    authority: Pubkey,
    guardian: Pubkey,
    vault_index: u8,
    maximum_amount: u64,
) -> Result<(), Box<dyn Error>> {
    let actions = decode_squads_policy_create_actions(&plan.create_instruction)?;
    let [action] = actions.as_slice() else {
        return Err(format!("{operation} policy is not exactly one create action").into());
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
        return Err(format!("{operation} decoded constraint header mismatch").into());
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
            return Err(format!("{operation} account constraint mismatch").into());
        }
    }
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
        return Err(format!("{operation} bounded data constraints mismatch").into());
    }
    Ok(())
}

fn policy_artifact(
    operation: &str,
    policy: &VoltrKaminoPolicyPlan,
    inner: &Instruction,
    guardian: Pubkey,
    vault_index: u8,
    strategy_id: Option<&str>,
) -> Result<PolicyArtifact, Box<dyn Error>> {
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner.clone());
    let execution = execute_program_interaction_policy_instruction(
        policy.policy,
        guardian,
        vault_index,
        vec![compiled],
        vec![0],
        transaction_accounts,
    );
    Ok(PolicyArtifact {
        strategy_id: strategy_id.map(str::to_owned),
        operation: operation.into(),
        seed: policy.policy_seed.to_string(),
        policy: policy.policy.to_string(),
        constrained_account_indexes: policy.constrained_account_indexes.clone(),
        inner_instruction_data_sha256: lower_hex(&Sha256::digest(&inner.data)),
        policy_create: instruction_artifact(&policy.create_instruction),
        policy_create_packet_bytes: packet_bytes(
            &policy.create_instruction,
            policy.create_instruction.accounts[1].pubkey,
        )?,
        manager_execution: instruction_artifact(&execution),
    })
}

fn instruction_artifact(instruction: &Instruction) -> InstructionArtifact {
    InstructionArtifact {
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
        data_length: instruction.data.len(),
        data_base64: BASE64.encode(&instruction.data),
        data_sha256: lower_hex(&Sha256::digest(&instruction.data)),
    }
}

fn packet_bytes(instruction: &Instruction, payer: Pubkey) -> Result<usize, Box<dyn Error>> {
    let message = v0::Message::try_compile(
        &payer,
        std::slice::from_ref(instruction),
        &[],
        Hash::new_unique(),
    )?;
    let transaction = VersionedTransaction {
        signatures: vec![Signature::default(); usize::from(message.header.num_required_signatures)],
        message: VersionedMessage::V0(message),
    };
    Ok(bincode::serialize(&transaction)?.len())
}

fn decode_instruction(
    value: &CanonicalInstruction,
    expected_labels: &[&str],
) -> Result<Instruction, Box<dyn Error>> {
    if value.accounts.len() != expected_labels.len() {
        return Err("canonical instruction account count mismatch".into());
    }
    let data = BASE64.decode(&value.data_base64)?;
    if data.len() != value.data_length
        || lower_hex(&data) != value.data_hex
        || lower_hex(&Sha256::digest(&data)) != value.data_sha256
    {
        return Err("canonical instruction data encodings disagree".into());
    }
    let mut accounts = Vec::with_capacity(value.accounts.len());
    for (position, (account, expected_label)) in
        value.accounts.iter().zip(expected_labels).enumerate()
    {
        if account.index != position || account.label != *expected_label {
            return Err(format!("non-canonical account label/index at {position}").into());
        }
        accounts.push(AccountMeta {
            pubkey: parse_pubkey(&account.address, "instruction account")?,
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

fn require_account(
    instruction: &Instruction,
    index: usize,
    expected: &str,
    label: &str,
) -> Result<(), Box<dyn Error>> {
    if instruction
        .accounts
        .get(index)
        .map(|account| account.pubkey)
        != Some(parse_pubkey(expected, label)?)
    {
        return Err(format!("{label} mismatch").into());
    }
    Ok(())
}

fn read_manifest(path: &std::path::Path) -> Result<RuntimeManifest, Box<dyn Error>> {
    Ok(serde_json::from_slice(&read_bytes(path)?)?)
}

fn read_bytes(path: &std::path::Path) -> Result<Vec<u8>, Box<dyn Error>> {
    let mut raw = Vec::new();
    if path == std::path::Path::new("-") {
        std::io::stdin().read_to_end(&mut raw)?;
    } else {
        raw = fs::read(path)?;
    }
    Ok(raw)
}

fn parse_cli() -> Result<Cli, Box<dyn Error>> {
    let mut manifests = Vec::new();
    let mut verify_artifact = None;
    let mut out = None;
    let mut args = std::env::args().skip(1);
    while let Some(flag) = args.next() {
        let value = args
            .next()
            .ok_or_else(|| format!("missing value for {flag}"))?;
        match flag.as_str() {
            "--manifest" => manifests.push(PathBuf::from(value)),
            "--verify-artifact" => verify_artifact = Some(PathBuf::from(value)),
            "--out" => out = Some(PathBuf::from(value)),
            _ => return Err(format!("unsupported argument {flag}").into()),
        }
    }
    if (manifests.is_empty() && verify_artifact.is_none())
        || (!manifests.is_empty() && verify_artifact.is_some())
    {
        return Err(
            "provide one or more --manifest values or exactly one --verify-artifact".into(),
        );
    }
    if verify_artifact.is_some() && out.is_some() {
        return Err("--out is not valid with --verify-artifact".into());
    }
    Ok(Cli {
        manifests,
        verify_artifact,
        out,
    })
}

fn parse_pubkey(value: &str, label: &str) -> Result<Pubkey, Box<dyn Error>> {
    Pubkey::from_str(value).map_err(|error| format!("invalid {label}: {error}").into())
}

fn artifact_hash(body: &ArtifactBody) -> Result<String, Box<dyn Error>> {
    digest_json(body)
}

fn digest_json<T: Serialize>(value: &T) -> Result<String, Box<dyn Error>> {
    Ok(lower_hex(&Sha256::digest(serde_json::to_vec(value)?)))
}

fn lower_hex(bytes: &[u8]) -> String {
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to String cannot fail");
    }
    output
}
