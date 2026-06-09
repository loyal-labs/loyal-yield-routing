use base64::{
    engine::general_purpose::{
        STANDARD as BASE64_STANDARD, STANDARD_NO_PAD as BASE64_STANDARD_NO_PAD,
    },
    Engine as _,
};
use borsh::BorshSerialize;
use clap::{Parser, ValueEnum, ValueHint};
use loyal_actions::{
    yield_route_universe_for_preset, JupiterSwapContract, KaminoStableRiskProfile,
    LoyalActionContext, RouteTopology, SwapLane, YieldRouteActionBuilder, YieldRouteActionSeeds,
    YieldRouteUniverse, YieldRouteUniversePreset, JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
    JUPITER_SWAP_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID, LOYAL_HUB_SWAP_PROGRAM_ID,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID, YIELD_ROUTE_DEPOSIT_ACTION_SEED, YIELD_ROUTE_SWAP_ACTION_SEED,
    YIELD_ROUTE_WITHDRAW_ACTION_SEED,
};
use serde_json::{json, Value};
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcSimulateTransactionAccountsConfig, RpcSimulateTransactionConfig},
    rpc_response::{Response, RpcSimulateTransactionResult},
};
use solana_sdk::{
    compute_budget::ComputeBudgetInstruction,
    instruction::{AccountMeta, Instruction, InstructionError},
    message::{v0, AddressLookupTableAccount, VersionedMessage},
    pubkey,
    pubkey::Pubkey,
    signature::{read_keypair_file, Keypair, Signature, Signer},
    transaction::{Transaction, TransactionError, VersionedTransaction},
};
use sqlx::{postgres::PgPoolOptions, PgPool};
use std::{
    env,
    path::{Path, PathBuf},
    time::Duration,
};
use thiserror::Error;

const MIGRATION_0001: &str =
    include_str!("../../loyal-yield-orchestrator/migrations/0001_loyal_yield_orchestration.sql");
const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
const SQUADS_SEED_SETTINGS: &[u8] = b"settings";
const SQUADS_SEED_SMART_ACCOUNT: &[u8] = b"smart_account";
const SQUADS_PROGRAM_CONFIG_SEED: &[u8] = b"program_config";
const SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR: [u8; 8] = [197, 102, 253, 231, 77, 84, 50, 17];
const SQUADS_EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR: [u8; 8] =
    [138, 209, 64, 163, 79, 67, 233, 76];
const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
const SQUADS_SETTINGS_ACTION_POLICY_REMOVE_TAG: u8 = 9;
const ANCHOR_INSTRUCTION_DID_NOT_DESERIALIZE: u32 = 102;
const PROGRAM_CONFIG_SMART_ACCOUNT_INDEX_OFFSET: usize = 8;
const PROGRAM_CONFIG_SMART_ACCOUNT_CREATION_FEE_OFFSET: usize = 56;
const PROGRAM_CONFIG_TREASURY_OFFSET: usize = 64;
const PROGRAM_CONFIG_MIN_LEN: usize = PROGRAM_CONFIG_TREASURY_OFFSET + 32;
const SQUADS_SINGLE_SIGNER_SETTINGS_ACCOUNT_SPACE: usize = 168;
const LAMPORTS_PER_SOL: u64 = 1_000_000_000;
const DEFAULT_VAULT_INDEX: u8 = 0;
const DEFAULT_MAX_FEE_BPS: u16 = 100;
const DEFAULT_HEAP_FRAME_BYTES: u32 = 256_000;
const DEFAULT_COMPUTE_UNIT_LIMIT: u32 = 1_400_000;
const YIELD_ROUTER_KEYPAIR_ENV: &str = "YIELD_ROUTER_KEYPAIR";
const SOLANA_SECRET_KEY_LENGTH: usize = 32;
const SOLANA_KEYPAIR_LENGTH: usize = 64;
const MAX_TRANSACTION_PACKET_BYTES: u64 = solana_sdk::packet::PACKET_DATA_SIZE as u64;
const LOYAL_HUB_AUTHORIZER: Pubkey = pubkey!("3uWi9x2SRpmjztkpkr2WWeBoVq3exjXG2YfDWLvm8KsQ");

const SHARED_CLUSTER_CONFIG: ClusterConfig = ClusterConfig {
    squads_smart_account_program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    jupiter_v6_program_id: pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"),
    loyal_hub_swap_program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
    loyal_hub_authorizer: LOYAL_HUB_AUTHORIZER,
    kamino_lend_program_id: KAMINO_LEND_PROGRAM_ID,
};

#[derive(Debug, Parser)]
#[command(about = "Create a Squads smart account and install a Loyal yield-route policy")]
struct Cli {
    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: String,
    #[arg(long, value_enum, default_value_t = Cluster::Devnet)]
    cluster: Cluster,
    #[arg(
        short = 'k',
        long = "keypair",
        value_name = "KP_FILE",
        value_hint = ValueHint::FilePath,
        help = "User Solana CLI keypair JSON file"
    )]
    user_keypair: PathBuf,
    #[arg(long, value_enum, default_value_t = RiskProfile::Safe)]
    risk_profile: RiskProfile,
    #[arg(long, value_enum, default_value_t = TopologyArg::AllInOne)]
    topology: TopologyArg,
    #[arg(long = "stable-mint", value_delimiter = ',')]
    stable_mints: Vec<Pubkey>,
    #[arg(long = "kamino-market", value_delimiter = ',')]
    kamino_markets: Vec<Pubkey>,
    #[arg(long = "kamino-liquidity-mint", value_delimiter = ',')]
    kamino_liquidity_mints: Vec<Pubkey>,
    #[arg(
        long = "swap-lane",
        value_enum,
        value_delimiter = ',',
        default_value = "jupiter"
    )]
    swap_lanes: Vec<SwapLaneArg>,
    #[arg(long = "same-mint-only")]
    same_mint_only: bool,
    #[arg(long, default_value_t = DEFAULT_VAULT_INDEX)]
    vault_index: u8,
    #[arg(long)]
    settings: Option<Pubkey>,
    #[arg(
        long,
        help = "Delegated signer pubkey to attach to created policies; defaults to YIELD_ROUTER_KEYPAIR pubkey"
    )]
    delegated_signer: Option<Pubkey>,
    #[arg(long)]
    smart_account_seed: Option<u128>,
    #[arg(long)]
    loyal_hub_authorizer: Option<Pubkey>,
    #[arg(long, default_value_t = DEFAULT_MAX_FEE_BPS)]
    max_fee_bps: u16,
    #[arg(long, default_value_t = YIELD_ROUTE_WITHDRAW_ACTION_SEED)]
    withdraw_action_seed: u64,
    #[arg(long, default_value_t = YIELD_ROUTE_SWAP_ACTION_SEED)]
    swap_action_seed: u64,
    #[arg(long, default_value_t = YIELD_ROUTE_DEPOSIT_ACTION_SEED)]
    deposit_action_seed: u64,
    #[arg(long, default_value_t = JUPITER_DEFAULT_MAX_SLIPPAGE_BPS)]
    jupiter_max_slippage_bps: u16,
    #[arg(long, default_value_t = DEFAULT_HEAP_FRAME_BYTES)]
    heap_frame_bytes: u32,
    #[arg(long, default_value_t = DEFAULT_COMPUTE_UNIT_LIMIT)]
    compute_unit_limit: u32,
    #[arg(long)]
    compute_unit_price_microlamports: Option<u64>,
    #[arg(long, env = "NEON_DATABASE_URL", hide_env_values = true)]
    postgres_url: Option<String>,
    #[arg(long)]
    skip_db: bool,
    #[arg(long)]
    dry_run: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
enum Cluster {
    Mainnet,
    Devnet,
}

impl Cluster {
    fn as_db_value(self) -> &'static str {
        match self {
            Self::Mainnet => "mainnet",
            Self::Devnet => "devnet",
        }
    }
}

/// Kamino stable reserve universe preset: Safe is narrowest, Medium expands
/// the allowlist, and Aggressive includes the broadest configured stable set.
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
enum RiskProfile {
    Safe,
    Medium,
    Aggressive,
}

impl RiskProfile {
    fn as_db_value(self) -> &'static str {
        match self {
            Self::Safe => "safe",
            Self::Medium => "medium",
            Self::Aggressive => "aggressive",
        }
    }

    fn as_preset(self) -> YieldRouteUniversePreset {
        let profile = match self {
            Self::Safe => KaminoStableRiskProfile::Safe,
            Self::Medium => KaminoStableRiskProfile::Medium,
            Self::Aggressive => KaminoStableRiskProfile::Aggressive,
        };
        YieldRouteUniversePreset::KaminoStable(profile)
    }
}

/// Cross-mint swap venue allowed by the route policy.
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
enum SwapLaneArg {
    Jupiter,
    // Policy creation is wired, but orchestrator quoting is not enabled yet.
    LoyalHub,
}

/// Policy topology for route execution: ThreeStep creates separate
/// withdraw/swap/deposit policies, CombinedKamino shares withdraw+deposit while
/// splitting swap, and AllInOne keeps the full route under one compact policy.
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
enum TopologyArg {
    ThreeStep,
    CombinedKamino,
    AllInOne,
}

impl TopologyArg {
    fn as_route_topology(self) -> RouteTopology {
        match self {
            Self::ThreeStep => RouteTopology::ThreeStep,
            Self::CombinedKamino => RouteTopology::CombinedKamino,
            Self::AllInOne => RouteTopology::AllInOne,
        }
    }
}

#[derive(Debug, Error)]
enum InitError {
    #[error("NEON_DATABASE_URL is required unless --skip-db or --dry-run is set")]
    MissingPostgresUrl,
    #[error("--settings and --smart-account-seed cannot be used together")]
    ConflictingSettingsInputs,
    #[error("Squads program config account is too short: expected at least {expected} bytes, got {actual}")]
    ShortProgramConfig { expected: usize, actual: usize },
    #[error("Squads smart-account index is already u128::MAX")]
    SmartAccountIndexOverflow,
    #[error("derived settings account already exists; rerun with --settings {settings} to install the policy on it")]
    SettingsAlreadyExists { settings: Pubkey },
    #[error(
        "existing --settings account {settings} is not owned by the Squads smart-account program"
    )]
    SettingsOwnerMismatch { settings: Pubkey },
    #[error("transaction {signature} was confirmed without a retrievable status slot")]
    MissingSignatureStatus { signature: Signature },
    #[error("failed to read user keypair file {path}: {message}")]
    ReadUserKeypair { path: PathBuf, message: String },
    #[error("{name} is not set")]
    MissingEnv { name: String },
    #[error("{name} must be hex, base58, base64, or a Solana keypair JSON array")]
    InvalidKeypairEncoding { name: String },
    #[error("{name} JSON keypair array is invalid: {message}")]
    InvalidJsonKeypair { name: String, message: String },
    #[error("{name} must decode to 32 or 64 bytes, got {lengths}")]
    InvalidKeypairLength { name: String, lengths: String },
    #[error("{name} bytes do not describe a valid Solana keypair")]
    InvalidKeypair { name: String },
    #[error(
        "{name} program id mismatch: loyal-actions has {actual}, cluster config has {expected}"
    )]
    ProgramIdMismatch {
        name: &'static str,
        actual: Pubkey,
        expected: Pubkey,
    },
    #[error("transaction is {actual} bytes, which exceeds the {max} byte Solana packet limit")]
    TransactionTooLarge { actual: u64, max: u64 },
    #[error("payer account {payer} does not exist on {cluster}; fund it with at least {minimum_lamports} lamports ({minimum_sol} SOL) and rerun")]
    PayerAccountNotFound {
        payer: Pubkey,
        cluster: String,
        minimum_lamports: u64,
        minimum_sol: String,
    },
    #[error("payer account {payer} has {balance_lamports} lamports on {cluster}, below the estimated startup requirement of {minimum_lamports} lamports ({minimum_sol} SOL)")]
    InsufficientPayerBalance {
        payer: Pubkey,
        cluster: String,
        balance_lamports: u64,
        minimum_lamports: u64,
        minimum_sol: String,
    },
    #[error("lamport funding estimate overflowed")]
    LamportEstimateOverflow,
    #[error(
        "Squads smart-account program {program_id} on {cluster} does not support policy settings actions; the preflight probe failed during instruction deserialization"
    )]
    PolicyActionsUnsupported { cluster: String, program_id: Pubkey },
    #[error("policy transaction {label} failed preflight simulation: {error}")]
    PolicyTransactionPreflightFailed { label: &'static str, error: String },
}

#[derive(Debug, Clone, Copy)]
struct ClusterConfig {
    squads_smart_account_program_id: Pubkey,
    jupiter_v6_program_id: Pubkey,
    loyal_hub_swap_program_id: Pubkey,
    loyal_hub_authorizer: Pubkey,
    kamino_lend_program_id: Pubkey,
}

#[derive(Debug)]
struct ProgramConfig {
    smart_account_index: u128,
    smart_account_creation_fee: u64,
    treasury: Pubkey,
}

#[derive(Debug)]
struct TransactionPlan {
    transactions: Vec<PlannedTransaction>,
    policy_actions: Vec<PolicyActionPlan>,
    creates_smart_account: bool,
}

#[derive(Debug)]
struct PlannedTransaction {
    label: &'static str,
    instructions: Vec<Instruction>,
    rent_accounts: Vec<RentAccountTarget>,
    policy_action: Option<PolicyActionPlan>,
}

#[derive(Debug)]
struct RentAccountTarget {
    label: &'static str,
    pubkey: Pubkey,
}

#[derive(Clone, Copy, Debug)]
struct PolicyActionPlan {
    label: &'static str,
    seed: u64,
    account: Pubkey,
    operation: PolicyActionOperation,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PolicyActionOperation {
    Create,
    Update,
}

impl PolicyActionOperation {
    fn as_json_value(self) -> &'static str {
        match self {
            Self::Create => "create",
            Self::Update => "update",
        }
    }
}

#[derive(Debug)]
struct SubmittedTransaction {
    label: &'static str,
    signature: Signature,
    slot: u64,
    policy_action: Option<PolicyActionPlan>,
}

#[derive(Debug)]
struct PolicySupportProbe {
    supports_policy_settings_actions: bool,
    report: Value,
}

#[derive(Debug)]
struct PolicyDbInput {
    signature: String,
    slot: u64,
    cluster: String,
    settings: String,
    authority: String,
    policy_seed: u64,
    policy_account: String,
    vault_index: u8,
    vault_pubkey: String,
    delegated_signers: Vec<String>,
    threshold: u16,
    route_modes: Vec<String>,
    stable_mints: Vec<String>,
    kamino_markets: Vec<String>,
    kamino_liquidity_mints: Vec<String>,
    universe_preset: Option<String>,
    risk_profile: Option<String>,
    swap_lanes: Value,
}

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("Error: {error}");
        std::process::exit(1);
    }
}

async fn run() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    if cli.settings.is_some() && cli.smart_account_seed.is_some() {
        return Err(InitError::ConflictingSettingsInputs.into());
    }
    let cluster_config = cluster_config(cli.cluster);
    ensure_cluster_config_matches_sdk(cluster_config)?;

    let user = user_keypair_from_file(&cli.user_keypair)?;
    let user_pubkey = user.pubkey();
    let router_pubkey = match cli.delegated_signer {
        Some(pubkey) => pubkey,
        None => keypair_from_env(YIELD_ROUTER_KEYPAIR_ENV)?.pubkey(),
    };
    let rpc = RpcClient::new(cli.rpc_url.clone());
    let db = connect_database(&cli).await?;

    let program_config = if cli.settings.is_none() {
        Some(fetch_squads_program_config(&rpc)?)
    } else {
        None
    };
    let smart_account_seed = match (
        cli.settings,
        cli.smart_account_seed,
        program_config.as_ref(),
    ) {
        (_, Some(seed), _) => Some(seed),
        (None, None, Some(config)) => Some(
            config
                .smart_account_index
                .checked_add(1)
                .ok_or(InitError::SmartAccountIndexOverflow)?,
        ),
        (Some(_), None, _) => None,
        (None, None, None) => unreachable!("program config is fetched when settings is omitted"),
    };

    let settings = cli
        .settings
        .unwrap_or_else(|| derive_squads_settings(smart_account_seed.expect("seed is set")).0);
    let vault = derive_squads_vault(&settings, cli.vault_index).0;

    if cli.settings.is_some() {
        ensure_existing_settings(&rpc, settings)?;
    } else if account_exists(&rpc, settings)? {
        return Err(InitError::SettingsAlreadyExists { settings }.into());
    }

    let swap_lanes = build_swap_lanes(&cli, cluster_config);
    let action_seeds = YieldRouteActionSeeds {
        withdraw: cli.withdraw_action_seed,
        swap: cli.swap_action_seed,
        deposit: cli.deposit_action_seed,
    };
    let action_setup = YieldRouteActionBuilder::new(
        LoyalActionContext {
            settings,
            authority: user_pubkey,
            delegated_signer: router_pubkey,
            account_index: cli.vault_index,
            vault,
        },
        build_route_universe(&cli),
    )
    .topology(cli.topology.as_route_topology())
    .swap_lanes(swap_lanes.clone())
    .seeds(action_seeds)
    .build()?;
    let mut policy_actions = policy_actions_for_setup(&action_setup, action_seeds);
    set_policy_action_operations(&rpc, &mut policy_actions)?;

    let transaction_plan = build_transaction_plan(
        &cli,
        user_pubkey,
        settings,
        smart_account_seed,
        &action_setup,
        &policy_actions,
        program_config.as_ref(),
    )?;
    ensure_plan_transactions_fit(&rpc, &transaction_plan, &user)?;
    if !cli.dry_run {
        ensure_payer_can_start_plan(
            &rpc,
            cli.cluster,
            user_pubkey,
            &transaction_plan,
            &user,
            program_config.as_ref(),
        )?;
    }
    let policy_support_probe = probe_policy_settings_action_support(
        &rpc,
        &user,
        user_pubkey,
        settings,
        smart_account_seed,
        program_config.as_ref(),
        &transaction_plan.policy_actions,
    )?;

    if cli.dry_run {
        let policy_simulation_skip_reason = if !policy_support_probe
            .supports_policy_settings_actions
        {
            Some("skipped because the Squads program cannot deserialize policy settings actions")
        } else if transaction_plan.creates_smart_account {
            Some(
                    "skipped because Solana dry-run simulation does not persist the simulated smart-account creation into later policy transactions",
                )
        } else {
            None
        };
        let dry_run = dry_run_plan_report(
            &rpc,
            &transaction_plan,
            &user,
            policy_simulation_skip_reason,
        )?;
        print_json(planned_output(
            &cli,
            cluster_config,
            user_pubkey,
            router_pubkey,
            settings,
            vault,
            smart_account_seed,
            &action_setup,
            &transaction_plan.policy_actions,
            None,
            None,
            Some(dry_run),
            Some(policy_support_probe.report),
        ))?;
        return Ok(());
    }

    if !policy_support_probe.supports_policy_settings_actions {
        return Err(InitError::PolicyActionsUnsupported {
            cluster: cli.cluster.as_db_value().to_owned(),
            program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        }
        .into());
    }
    ensure_existing_settings_policy_preflight(&rpc, &transaction_plan, &user)?;

    let submitted_transactions = send_and_confirm_plan(&rpc, &transaction_plan, &user)?;

    let db_result = if let Some(pool) = db.as_ref() {
        Some(
            upsert_policy_actions_and_vault(
                pool,
                &cli,
                user_pubkey,
                router_pubkey,
                settings,
                vault,
                &action_setup,
                &submitted_transactions,
            )
            .await?,
        )
    } else {
        None
    };

    print_json(planned_output(
        &cli,
        cluster_config,
        user_pubkey,
        router_pubkey,
        settings,
        vault,
        smart_account_seed,
        &action_setup,
        &transaction_plan.policy_actions,
        Some(&submitted_transactions),
        db_result,
        None,
        Some(policy_support_probe.report),
    ))?;
    Ok(())
}

async fn connect_database(cli: &Cli) -> Result<Option<PgPool>, Box<dyn std::error::Error>> {
    if cli.dry_run || cli.skip_db {
        return Ok(None);
    }
    let url = match cli.postgres_url.as_ref() {
        Some(url) => url.clone(),
        None => env::var("NEON_DATABASE_URL").map_err(|_| InitError::MissingPostgresUrl)?,
    };
    let pool = PgPoolOptions::new()
        .max_connections(2)
        .acquire_timeout(Duration::from_secs(10))
        .connect(&url)
        .await?;
    sqlx::raw_sql(MIGRATION_0001).execute(&pool).await?;
    Ok(Some(pool))
}

fn cluster_config(cluster: Cluster) -> ClusterConfig {
    match cluster {
        Cluster::Mainnet | Cluster::Devnet => SHARED_CLUSTER_CONFIG,
    }
}

fn ensure_cluster_config_matches_sdk(config: ClusterConfig) -> Result<(), InitError> {
    if SQUADS_SMART_ACCOUNT_PROGRAM_ID != config.squads_smart_account_program_id {
        return Err(InitError::ProgramIdMismatch {
            name: "Squads smart account",
            actual: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            expected: config.squads_smart_account_program_id,
        });
    }
    if LOYAL_HUB_SWAP_PROGRAM_ID != config.loyal_hub_swap_program_id {
        return Err(InitError::ProgramIdMismatch {
            name: "Loyal Hub swap",
            actual: LOYAL_HUB_SWAP_PROGRAM_ID,
            expected: config.loyal_hub_swap_program_id,
        });
    }
    if KAMINO_LEND_PROGRAM_ID != config.kamino_lend_program_id {
        return Err(InitError::ProgramIdMismatch {
            name: "Kamino lend",
            actual: KAMINO_LEND_PROGRAM_ID,
            expected: config.kamino_lend_program_id,
        });
    }
    Ok(())
}

fn build_route_universe(cli: &Cli) -> YieldRouteUniverse {
    if cli.stable_mints.is_empty()
        && cli.kamino_markets.is_empty()
        && cli.kamino_liquidity_mints.is_empty()
    {
        return yield_route_universe_for_preset(cli.risk_profile.as_preset());
    }

    let stable_mints = if cli.stable_mints.is_empty() {
        cli.kamino_liquidity_mints.clone()
    } else {
        cli.stable_mints.clone()
    };
    let kamino_liquidity_mints = if cli.kamino_liquidity_mints.is_empty() {
        stable_mints.clone()
    } else {
        cli.kamino_liquidity_mints.clone()
    };

    YieldRouteUniverse::new(
        stable_mints,
        cli.kamino_markets.clone(),
        kamino_liquidity_mints,
    )
}

fn policy_actions_for_setup(
    setup: &loyal_actions::YieldRouteActionSetup,
    seeds: YieldRouteActionSeeds,
) -> Vec<PolicyActionPlan> {
    let candidates = [
        PolicyActionPlan {
            label: "withdraw_policy",
            seed: seeds.withdraw,
            account: setup.accounts.withdraw,
            operation: PolicyActionOperation::Create,
        },
        PolicyActionPlan {
            label: "swap_policy",
            seed: seeds.swap,
            account: setup.accounts.swap,
            operation: PolicyActionOperation::Create,
        },
        PolicyActionPlan {
            label: "deposit_policy",
            seed: seeds.deposit,
            account: setup.accounts.deposit,
            operation: PolicyActionOperation::Create,
        },
    ];

    let mut actions = Vec::new();
    for candidate in candidates {
        if actions
            .iter()
            .any(|existing: &PolicyActionPlan| existing.account == candidate.account)
        {
            continue;
        }
        actions.push(candidate);
    }
    actions
}

fn set_policy_action_operations(
    rpc: &RpcClient,
    actions: &mut [PolicyActionPlan],
) -> Result<(), Box<dyn std::error::Error>> {
    for action in actions {
        action.operation = if account_exists(rpc, action.account)? {
            PolicyActionOperation::Update
        } else {
            PolicyActionOperation::Create
        };
    }
    Ok(())
}

fn policy_action_json(action: PolicyActionPlan) -> Value {
    json!({
        "label": action.label,
        "seed": action.seed,
        "account": action.account.to_string(),
        "operation": action.operation.as_json_value(),
    })
}

fn build_transaction_plan(
    cli: &Cli,
    user_pubkey: Pubkey,
    settings: Pubkey,
    smart_account_seed: Option<u128>,
    setup: &loyal_actions::YieldRouteActionSetup,
    policy_actions: &[PolicyActionPlan],
    program_config: Option<&ProgramConfig>,
) -> Result<TransactionPlan, Box<dyn std::error::Error>> {
    let creates_smart_account = program_config.is_some();
    let mut transactions = Vec::new();
    if let Some(config) = program_config {
        let seed = smart_account_seed.expect("seed is set before smart-account creation");
        let mut instructions = compute_budget_instructions(cli);
        instructions.push(create_squads_smart_account_instruction(
            user_pubkey,
            &[user_pubkey],
            seed,
            config.treasury,
        ));
        transactions.push(PlannedTransaction {
            label: "create_smart_account",
            instructions,
            rent_accounts: vec![RentAccountTarget {
                label: "squads_settings",
                pubkey: settings,
            }],
            policy_action: None,
        });
    }

    for ((create_instruction, update_instruction), action) in setup
        .instructions
        .iter()
        .zip(setup.update_instructions.iter())
        .zip(policy_actions.iter())
    {
        let mut instructions = compute_budget_instructions(cli);
        instructions.push(match action.operation {
            PolicyActionOperation::Create => create_instruction.clone(),
            PolicyActionOperation::Update => update_instruction.clone(),
        });
        let rent_accounts = match action.operation {
            PolicyActionOperation::Create => vec![RentAccountTarget {
                label: action.label,
                pubkey: action.account,
            }],
            PolicyActionOperation::Update => Vec::new(),
        };
        transactions.push(PlannedTransaction {
            label: action.label,
            instructions,
            rent_accounts,
            policy_action: Some(*action),
        });
    }

    Ok(TransactionPlan {
        transactions,
        policy_actions: policy_actions.to_vec(),
        creates_smart_account,
    })
}

fn compute_budget_instructions(cli: &Cli) -> Vec<Instruction> {
    let mut instructions = vec![
        ComputeBudgetInstruction::request_heap_frame(cli.heap_frame_bytes),
        ComputeBudgetInstruction::set_compute_unit_limit(cli.compute_unit_limit),
    ];
    if let Some(price) = cli.compute_unit_price_microlamports {
        instructions.push(ComputeBudgetInstruction::set_compute_unit_price(price));
    }
    instructions
}

fn signed_transaction(
    rpc: &RpcClient,
    instructions: &[Instruction],
    signer: &impl Signer,
) -> Result<Transaction, Box<dyn std::error::Error>> {
    let blockhash = rpc.get_latest_blockhash()?;
    Ok(Transaction::new_signed_with_payer(
        instructions,
        Some(&signer.pubkey()),
        &[signer],
        blockhash,
    ))
}

fn ensure_plan_transactions_fit(
    rpc: &RpcClient,
    plan: &TransactionPlan,
    signer: &impl Signer,
) -> Result<(), Box<dyn std::error::Error>> {
    for transaction in &plan.transactions {
        let signed = signed_transaction(rpc, &transaction.instructions, signer)?;
        ensure_transaction_fits_packet(&signed)?;
    }
    Ok(())
}

fn ensure_existing_settings_policy_preflight(
    rpc: &RpcClient,
    plan: &TransactionPlan,
    signer: &impl Signer,
) -> Result<(), Box<dyn std::error::Error>> {
    if plan.creates_smart_account {
        return Ok(());
    }

    for transaction in &plan.transactions {
        if transaction.policy_action.is_none() {
            continue;
        }

        let signed = signed_transaction(rpc, &transaction.instructions, signer)?;
        let simulation = rpc.simulate_transaction_with_config(
            &signed,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                inner_instructions: true,
                ..RpcSimulateTransactionConfig::default()
            },
        )?;

        if let Some(error) = simulation.value.err {
            return Err(InitError::PolicyTransactionPreflightFailed {
                label: transaction.label,
                error: format!("{error:?}"),
            }
            .into());
        }
    }

    Ok(())
}

fn ensure_payer_can_start_plan(
    rpc: &RpcClient,
    cluster: Cluster,
    payer: Pubkey,
    plan: &TransactionPlan,
    signer: &impl Signer,
    program_config: Option<&ProgramConfig>,
) -> Result<(), Box<dyn std::error::Error>> {
    let minimum_lamports = estimate_startup_lamports(rpc, plan, signer, program_config)?;
    let Some(balance_lamports) = account_lamports(rpc, payer)? else {
        return Err(InitError::PayerAccountNotFound {
            payer,
            cluster: cluster.as_db_value().to_owned(),
            minimum_lamports,
            minimum_sol: lamports_to_sol_string(minimum_lamports),
        }
        .into());
    };

    if balance_lamports < minimum_lamports {
        return Err(InitError::InsufficientPayerBalance {
            payer,
            cluster: cluster.as_db_value().to_owned(),
            balance_lamports,
            minimum_lamports,
            minimum_sol: lamports_to_sol_string(minimum_lamports),
        }
        .into());
    }

    Ok(())
}

fn estimate_startup_lamports(
    rpc: &RpcClient,
    plan: &TransactionPlan,
    signer: &impl Signer,
    program_config: Option<&ProgramConfig>,
) -> Result<u64, Box<dyn std::error::Error>> {
    let mut lamports = 0u64;
    for transaction in &plan.transactions {
        let signed = signed_transaction(rpc, &transaction.instructions, signer)?;
        lamports = checked_add_lamports(lamports, rpc.get_fee_for_message(signed.message())?)?;
    }

    if plan.creates_smart_account {
        lamports = checked_add_lamports(
            lamports,
            rpc.get_minimum_balance_for_rent_exemption(
                SQUADS_SINGLE_SIGNER_SETTINGS_ACCOUNT_SPACE,
            )?,
        )?;
        if let Some(config) = program_config {
            lamports = checked_add_lamports(lamports, config.smart_account_creation_fee)?;
        }
    }

    Ok(lamports)
}

fn checked_add_lamports(lhs: u64, rhs: u64) -> Result<u64, InitError> {
    lhs.checked_add(rhs)
        .ok_or(InitError::LamportEstimateOverflow)
}

fn account_lamports(
    rpc: &RpcClient,
    pubkey: Pubkey,
) -> Result<Option<u64>, Box<dyn std::error::Error>> {
    Ok(rpc
        .get_account_with_commitment(&pubkey, rpc.commitment())?
        .value
        .map(|account| account.lamports))
}

fn lamports_to_sol_string(lamports: u64) -> String {
    format!(
        "{}.{:09}",
        lamports / LAMPORTS_PER_SOL,
        lamports % LAMPORTS_PER_SOL
    )
}

fn send_and_confirm_plan(
    rpc: &RpcClient,
    plan: &TransactionPlan,
    signer: &impl Signer,
) -> Result<Vec<SubmittedTransaction>, Box<dyn std::error::Error>> {
    let mut submitted = Vec::with_capacity(plan.transactions.len());
    for transaction in &plan.transactions {
        let signed = signed_transaction(rpc, &transaction.instructions, signer)?;
        ensure_transaction_fits_packet(&signed)?;
        let signature = send_and_confirm(rpc, &signed)?;
        let slot = confirmed_signature_slot(rpc, signature)?;
        submitted.push(SubmittedTransaction {
            label: transaction.label,
            signature,
            slot,
            policy_action: transaction.policy_action,
        });
    }
    Ok(submitted)
}

fn ensure_transaction_fits_packet(
    transaction: &Transaction,
) -> Result<(), Box<dyn std::error::Error>> {
    let actual = transaction_size_bytes(transaction)?;
    if actual > MAX_TRANSACTION_PACKET_BYTES {
        return Err(InitError::TransactionTooLarge {
            actual,
            max: MAX_TRANSACTION_PACKET_BYTES,
        }
        .into());
    }
    Ok(())
}

fn transaction_size_bytes(transaction: &Transaction) -> Result<u64, Box<dyn std::error::Error>> {
    Ok(bincode::serialized_size(transaction)?)
}

fn versioned_transaction_size_bytes(
    transaction: &VersionedTransaction,
) -> Result<u64, Box<dyn std::error::Error>> {
    Ok(bincode::serialized_size(transaction)?)
}

fn user_keypair_from_file(path: &Path) -> Result<Keypair, InitError> {
    read_keypair_file(path).map_err(|error| InitError::ReadUserKeypair {
        path: path.to_path_buf(),
        message: error.to_string(),
    })
}

fn keypair_from_env(name: &str) -> Result<Keypair, InitError> {
    let value = env::var(name).map_err(|_| InitError::MissingEnv {
        name: name.to_owned(),
    })?;
    keypair_from_secret_string(name, &value)
}

fn keypair_from_secret_string(name: &str, value: &str) -> Result<Keypair, InitError> {
    let value = value.trim();
    if value.starts_with('[') {
        let bytes = serde_json::from_str::<Vec<u8>>(value).map_err(|error| {
            InitError::InvalidJsonKeypair {
                name: name.to_owned(),
                message: error.to_string(),
            }
        })?;
        return keypair_from_bytes(name, bytes);
    }

    let mut decoded_lengths = Vec::new();
    let mut decoded_invalid_keypair = false;
    for bytes in decode_secret_candidates(value) {
        match keypair_from_bytes(name, bytes) {
            Ok(keypair) => return Ok(keypair),
            Err(InitError::InvalidKeypairLength { lengths, .. }) => decoded_lengths.push(lengths),
            Err(InitError::InvalidKeypair { .. }) => decoded_invalid_keypair = true,
            Err(error) => return Err(error),
        }
    }

    if decoded_lengths.is_empty() {
        return Err(InitError::InvalidKeypairEncoding {
            name: name.to_owned(),
        });
    }

    if decoded_invalid_keypair {
        return Err(InitError::InvalidKeypair {
            name: name.to_owned(),
        });
    }

    decoded_lengths.sort();
    decoded_lengths.dedup();
    Err(InitError::InvalidKeypairLength {
        name: name.to_owned(),
        lengths: decoded_lengths.join(", "),
    })
}

fn keypair_from_bytes(name: &str, bytes: Vec<u8>) -> Result<Keypair, InitError> {
    match bytes.len() {
        SOLANA_SECRET_KEY_LENGTH => {
            let mut seed = [0u8; SOLANA_SECRET_KEY_LENGTH];
            seed.copy_from_slice(&bytes);
            Ok(Keypair::new_from_array(seed))
        }
        SOLANA_KEYPAIR_LENGTH => {
            Keypair::try_from(bytes.as_slice()).map_err(|_| InitError::InvalidKeypair {
                name: name.to_owned(),
            })
        }
        length => Err(InitError::InvalidKeypairLength {
            name: name.to_owned(),
            lengths: length.to_string(),
        }),
    }
}

fn decode_secret_candidates(value: &str) -> Vec<Vec<u8>> {
    let mut candidates = Vec::new();
    if let Ok(bytes) = decode_hex(value) {
        candidates.push(bytes);
    }
    if let Ok(bytes) = bs58::decode(value).into_vec() {
        candidates.push(bytes);
    }
    if let Ok(bytes) = BASE64_STANDARD.decode(value) {
        candidates.push(bytes);
    }
    if let Ok(bytes) = BASE64_STANDARD_NO_PAD.decode(value) {
        candidates.push(bytes);
    }
    candidates
}

fn decode_hex(value: &str) -> Result<Vec<u8>, ()> {
    let value = value.trim();
    let value = value
        .strip_prefix("0x")
        .or_else(|| value.strip_prefix("0X"))
        .unwrap_or(value);

    if value.len() % 2 != 0 || !value.as_bytes().iter().all(u8::is_ascii_hexdigit) {
        return Err(());
    }

    value
        .as_bytes()
        .chunks_exact(2)
        .map(|chunk| {
            let high = hex_nibble(chunk[0])?;
            let low = hex_nibble(chunk[1])?;
            Ok((high << 4) | low)
        })
        .collect()
}

fn hex_nibble(byte: u8) -> Result<u8, ()> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        b'A'..=b'F' => Ok(byte - b'A' + 10),
        _ => Err(()),
    }
}

fn fetch_squads_program_config(
    rpc: &RpcClient,
) -> Result<ProgramConfig, Box<dyn std::error::Error>> {
    let account = rpc.get_account(&derive_squads_program_config())?;
    if account.data.len() < PROGRAM_CONFIG_MIN_LEN {
        return Err(InitError::ShortProgramConfig {
            expected: PROGRAM_CONFIG_MIN_LEN,
            actual: account.data.len(),
        }
        .into());
    }

    let mut seed_bytes = [0u8; 16];
    seed_bytes.copy_from_slice(
        &account.data[PROGRAM_CONFIG_SMART_ACCOUNT_INDEX_OFFSET
            ..PROGRAM_CONFIG_SMART_ACCOUNT_INDEX_OFFSET + 16],
    );
    let mut creation_fee_bytes = [0u8; 8];
    creation_fee_bytes.copy_from_slice(
        &account.data[PROGRAM_CONFIG_SMART_ACCOUNT_CREATION_FEE_OFFSET
            ..PROGRAM_CONFIG_SMART_ACCOUNT_CREATION_FEE_OFFSET + 8],
    );
    let mut treasury_bytes = [0u8; 32];
    treasury_bytes.copy_from_slice(
        &account.data[PROGRAM_CONFIG_TREASURY_OFFSET..PROGRAM_CONFIG_TREASURY_OFFSET + 32],
    );

    Ok(ProgramConfig {
        smart_account_index: u128::from_le_bytes(seed_bytes),
        smart_account_creation_fee: u64::from_le_bytes(creation_fee_bytes),
        treasury: Pubkey::new_from_array(treasury_bytes),
    })
}

fn ensure_existing_settings(
    rpc: &RpcClient,
    settings: Pubkey,
) -> Result<(), Box<dyn std::error::Error>> {
    let account = rpc.get_account(&settings)?;
    if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return Err(InitError::SettingsOwnerMismatch { settings }.into());
    }
    Ok(())
}

fn account_exists(rpc: &RpcClient, pubkey: Pubkey) -> Result<bool, Box<dyn std::error::Error>> {
    Ok(rpc
        .get_account_with_commitment(&pubkey, rpc.commitment())?
        .value
        .is_some())
}

fn send_and_confirm(
    rpc: &RpcClient,
    transaction: &Transaction,
) -> Result<Signature, Box<dyn std::error::Error>> {
    Ok(rpc.send_and_confirm_transaction(transaction)?)
}

fn confirmed_signature_slot(
    rpc: &RpcClient,
    signature: Signature,
) -> Result<u64, Box<dyn std::error::Error>> {
    let statuses = rpc.get_signature_statuses(&[signature])?;
    if let Some(status) = statuses.value.into_iter().flatten().next() {
        return Ok(status.slot);
    }
    Err(InitError::MissingSignatureStatus { signature }.into())
}

fn probe_policy_settings_action_support(
    rpc: &RpcClient,
    payer: &Keypair,
    payer_pubkey: Pubkey,
    settings: Pubkey,
    smart_account_seed: Option<u128>,
    program_config: Option<&ProgramConfig>,
    policy_actions: &[PolicyActionPlan],
) -> Result<PolicySupportProbe, Box<dyn std::error::Error>> {
    let probe_policy_account = policy_actions
        .first()
        .map(|action| action.account)
        .unwrap_or_else(Pubkey::new_unique);
    let mut instructions = Vec::new();
    if let Some(config) = program_config {
        instructions.push(create_squads_smart_account_instruction(
            payer_pubkey,
            &[payer_pubkey],
            smart_account_seed.expect("seed is set before smart-account creation"),
            config.treasury,
        ));
    }
    instructions.push(create_squads_policy_remove_probe_instruction(
        settings,
        payer_pubkey,
        probe_policy_account,
    ));

    let transaction = signed_transaction(rpc, &instructions, payer)?;
    let transaction_report = transaction_size_report(&transaction, &instructions, payer)?;
    let legacy_fits = transaction_report["legacy"]["fitsPacket"]
        .as_bool()
        .unwrap_or(false);
    if !legacy_fits {
        return Ok(PolicySupportProbe {
            supports_policy_settings_actions: false,
            report: json!({
                "checked": true,
                "supportsPolicySettingsActions": false,
                "probe": {
                    "settings": settings.to_string(),
                    "policyAccount": probe_policy_account.to_string(),
                    "transaction": transaction_report,
                    "simulation": {
                        "attempted": false,
                        "ok": false,
                        "error": "policy support probe transaction exceeds Solana packet limits",
                    },
                },
            }),
        });
    }

    let simulation = rpc.simulate_transaction_with_config(
        &transaction,
        RpcSimulateTransactionConfig {
            sig_verify: true,
            inner_instructions: true,
            ..RpcSimulateTransactionConfig::default()
        },
    )?;
    let failed_during_deserialization =
        simulation_has_instruction_deserialize_error(&simulation.value);
    let supports_policy_settings_actions = !failed_during_deserialization;
    Ok(PolicySupportProbe {
        supports_policy_settings_actions,
        report: json!({
            "checked": true,
            "supportsPolicySettingsActions": supports_policy_settings_actions,
            "probe": {
                "settings": settings.to_string(),
                "policyAccount": probe_policy_account.to_string(),
                "usesSimulatedSmartAccountCreate": program_config.is_some(),
                "transaction": transaction_report,
                "simulation": {
                    "attempted": true,
                    "transactionFormat": "legacy",
                    "slot": simulation.context.slot,
                    "ok": simulation.value.err.is_none(),
                    "error": simulation.value.err.as_ref().map(|err| format!("{err:?}")),
                    "unitsConsumed": simulation.value.units_consumed,
                    "loadedAccountsDataSize": simulation.value.loaded_accounts_data_size,
                    "logs": simulation.value.logs,
                    "failedDuringInstructionDeserialization": failed_during_deserialization,
                },
            },
        }),
    })
}

fn simulation_has_instruction_deserialize_error(result: &RpcSimulateTransactionResult) -> bool {
    if matches!(
        result.err.as_ref(),
        Some(TransactionError::InstructionError(
            _,
            InstructionError::Custom(ANCHOR_INSTRUCTION_DID_NOT_DESERIALIZE)
        ))
    ) {
        return true;
    }
    result.logs.as_ref().is_some_and(|logs| {
        logs.iter()
            .any(|log| log.contains("InstructionDidNotDeserialize"))
    })
}

fn dry_run_report_without_simulation(
    rpc: &RpcClient,
    transaction: &Transaction,
    instructions: &[Instruction],
    rent_accounts: &[RentAccountTarget],
    payer: &Keypair,
    skipped_reason: &'static str,
) -> Result<Value, Box<dyn std::error::Error>> {
    let payer_pubkey = payer.pubkey();
    let transaction_report = transaction_size_report(transaction, instructions, payer)?;
    let legacy_fits = transaction_report["legacy"]["fitsPacket"]
        .as_bool()
        .unwrap_or(false);
    let fee_lamports = legacy_fits
        .then(|| rpc.get_fee_for_message(transaction.message()))
        .transpose()?;
    let payer_balance_lamports = rpc.get_balance(&payer_pubkey)?;

    Ok(json!({
        "transaction": transaction_report,
        "fee": {
            "lamports": fee_lamports,
        },
        "rent": {
            "lamports": null,
            "accounts": rent_accounts
                .iter()
                .map(|target| json!({
                    "label": target.label,
                    "pubkey": target.pubkey.to_string(),
                    "lamports": null,
                    "owner": null,
                    "space": null,
                }))
                .collect::<Vec<_>>(),
            "skippedReason": skipped_reason,
        },
        "total": {
            "estimatedLamports": null,
        },
        "payer": {
            "pubkey": payer_pubkey.to_string(),
            "balanceLamports": payer_balance_lamports,
            "hasEnoughForEstimate": null,
            "estimatedRemainingLamports": null,
        },
        "simulation": {
            "attempted": false,
            "ok": false,
            "error": skipped_reason,
        },
    }))
}

fn dry_run_plan_report(
    rpc: &RpcClient,
    plan: &TransactionPlan,
    payer: &Keypair,
    policy_simulation_skip_reason: Option<&'static str>,
) -> Result<Value, Box<dyn std::error::Error>> {
    let mut reports = Vec::with_capacity(plan.transactions.len());
    for planned in &plan.transactions {
        let transaction = signed_transaction(rpc, &planned.instructions, payer)?;
        let report = if let (Some(_), Some(skipped_reason)) =
            (planned.policy_action, policy_simulation_skip_reason)
        {
            dry_run_report_without_simulation(
                rpc,
                &transaction,
                &planned.instructions,
                &planned.rent_accounts,
                payer,
                skipped_reason,
            )?
        } else {
            dry_run_report(
                rpc,
                &transaction,
                &planned.instructions,
                &planned.rent_accounts,
                payer,
            )?
        };
        reports.push(json!({
            "label": planned.label,
            "policyAction": planned.policy_action.map(policy_action_json),
            "report": report,
        }));
    }

    let fee_lamports = sum_optional_lamports(&reports, &["report", "fee", "lamports"]);
    let rent_lamports = sum_optional_lamports(&reports, &["report", "rent", "lamports"]);
    let estimated_total_lamports = match (fee_lamports, rent_lamports) {
        (Some(fee), Some(rent)) => Some(fee + rent),
        _ => None,
    };

    Ok(json!({
        "transactionPacking": "split_transactions",
        "transactionCount": plan.transactions.len(),
        "createsSmartAccount": plan.creates_smart_account,
        "fee": {
            "lamports": fee_lamports,
        },
        "rent": {
            "lamports": rent_lamports,
        },
        "total": {
            "estimatedLamports": estimated_total_lamports,
        },
        "transactions": reports,
    }))
}

fn sum_optional_lamports(reports: &[Value], path: &[&str]) -> Option<u64> {
    let mut total = 0u64;
    for report in reports {
        let mut value = report;
        for key in path {
            value = &value[*key];
        }
        let lamports = value.as_u64()?;
        total = total.checked_add(lamports)?;
    }
    Some(total)
}

fn dry_run_report(
    rpc: &RpcClient,
    transaction: &Transaction,
    instructions: &[Instruction],
    rent_accounts: &[RentAccountTarget],
    payer: &Keypair,
) -> Result<Value, Box<dyn std::error::Error>> {
    let payer_pubkey = payer.pubkey();
    let transaction_report = transaction_size_report(transaction, instructions, payer)?;
    let legacy_fits = transaction_report["legacy"]["fitsPacket"]
        .as_bool()
        .unwrap_or(false);
    let v0_without_lookup_fits = transaction_report["v0WithoutLookupTables"]["fitsPacket"]
        .as_bool()
        .unwrap_or(false);
    let addresses = rent_accounts
        .iter()
        .map(|account| account.pubkey.to_string())
        .collect::<Vec<_>>();
    let simulation_config = || RpcSimulateTransactionConfig {
        sig_verify: true,
        accounts: (!addresses.is_empty()).then_some(RpcSimulateTransactionAccountsConfig {
            encoding: None,
            addresses: addresses.clone(),
        }),
        inner_instructions: true,
        ..RpcSimulateTransactionConfig::default()
    };
    let success_report =
        |fee_lamports: u64,
         transaction_format: &str,
         simulation: Response<RpcSimulateTransactionResult>| {
            let simulated_rent_accounts = rent_accounts
                .iter()
                .enumerate()
                .map(|(index, target)| {
                    let account = simulation
                        .value
                        .accounts
                        .as_ref()
                        .and_then(|accounts| accounts.get(index))
                        .and_then(|account| account.as_ref());
                    json!({
                        "label": target.label,
                        "pubkey": target.pubkey.to_string(),
                        "lamports": account.map(|account| account.lamports),
                        "owner": account.map(|account| account.owner.clone()),
                        "space": account.and_then(|account| account.space),
                    })
                })
                .collect::<Vec<_>>();
            let rent_lamports = simulation.value.accounts.as_ref().and_then(|accounts| {
                if accounts.len() != rent_accounts.len() {
                    return None;
                }
                accounts
                    .iter()
                    .map(|account| account.as_ref().map(|account| account.lamports))
                    .sum::<Option<u64>>()
            });
            let estimated_total_lamports = rent_lamports.map(|rent| rent + fee_lamports);
            let payer_balance_lamports = rpc.get_balance(&payer_pubkey)?;

            Ok::<Value, Box<dyn std::error::Error>>(json!({
                "transaction": transaction_report.clone(),
                "fee": {
                    "lamports": fee_lamports,
                },
                "rent": {
                    "lamports": rent_lamports,
                    "accounts": simulated_rent_accounts,
                },
                "total": {
                    "estimatedLamports": estimated_total_lamports,
                },
                "payer": {
                    "pubkey": payer_pubkey.to_string(),
                    "balanceLamports": payer_balance_lamports,
                    "hasEnoughForEstimate": estimated_total_lamports.map(|total| payer_balance_lamports >= total),
                    "estimatedRemainingLamports": estimated_total_lamports.map(|total| payer_balance_lamports.saturating_sub(total)),
                },
                "simulation": {
                    "attempted": true,
                    "transactionFormat": transaction_format,
                    "slot": simulation.context.slot,
                    "ok": simulation.value.err.is_none(),
                    "error": simulation.value.err.as_ref().map(|err| format!("{err:?}")),
                    "unitsConsumed": simulation.value.units_consumed,
                    "loadedAccountsDataSize": simulation.value.loaded_accounts_data_size,
                    "logs": simulation.value.logs,
                },
            }))
        };

    if legacy_fits {
        let fee_lamports = rpc.get_fee_for_message(transaction.message())?;
        let simulation = rpc.simulate_transaction_with_config(transaction, simulation_config())?;
        return success_report(fee_lamports, "legacy", simulation);
    }

    if v0_without_lookup_fits {
        let v0_transaction = signed_v0_transaction(
            &payer_pubkey,
            instructions,
            &[],
            transaction.message.recent_blockhash,
            payer,
        )?;
        let v0_message = match &v0_transaction.message {
            VersionedMessage::V0(message) => message,
            VersionedMessage::Legacy(_) => unreachable!("signed_v0_transaction builds v0 messages"),
        };
        let fee_lamports = rpc.get_fee_for_message(v0_message)?;
        let simulation =
            rpc.simulate_transaction_with_config(&v0_transaction, simulation_config())?;
        return success_report(fee_lamports, "v0_without_lookup_tables", simulation);
    }

    Ok(oversized_dry_run_report(transaction_report, payer_pubkey))
}

fn transaction_size_report(
    transaction: &Transaction,
    instructions: &[Instruction],
    payer: &Keypair,
) -> Result<Value, Box<dyn std::error::Error>> {
    let blockhash = transaction.message.recent_blockhash;
    let legacy_transaction_bytes = transaction_size_bytes(transaction)?;
    let legacy_message_bytes = transaction.message.serialize().len() as u64;
    let v0_without_lookup =
        signed_v0_transaction(&payer.pubkey(), instructions, &[], blockhash, payer)
            .ok()
            .map(|transaction| versioned_transaction_report(&transaction));
    let hypothetical_lookup_table =
        hypothetical_lookup_table_account(&payer.pubkey(), instructions);
    let hypothetical_lookup_address_count = hypothetical_lookup_table.addresses.len();
    let v0_with_hypothetical_lookup = (hypothetical_lookup_address_count > 0)
        .then(|| {
            signed_v0_transaction(
                &payer.pubkey(),
                instructions,
                &[hypothetical_lookup_table],
                blockhash,
                payer,
            )
            .ok()
            .map(|transaction| versioned_transaction_report(&transaction))
        })
        .flatten();
    let instruction_data_bytes = instructions
        .iter()
        .map(|instruction| instruction.data.len())
        .sum::<usize>() as u64;

    Ok(json!({
        "format": "single_transaction",
        "maxPacketBytes": MAX_TRANSACTION_PACKET_BYTES,
        "legacy": {
            "instructionCount": transaction.message.instructions.len(),
            "accountKeyCount": transaction.message.account_keys.len(),
            "signatureCount": transaction.signatures.len(),
            "messageBytes": legacy_message_bytes,
            "transactionBytes": legacy_transaction_bytes,
            "fitsPacket": legacy_message_bytes <= MAX_TRANSACTION_PACKET_BYTES
                && legacy_transaction_bytes <= MAX_TRANSACTION_PACKET_BYTES,
        },
        "v0WithoutLookupTables": v0_without_lookup,
        "v0WithHypotheticalLookupTable": {
            "lookupAddressCount": hypothetical_lookup_address_count,
            "report": v0_with_hypothetical_lookup,
            "note": "Size estimate only. A real on-chain address lookup table must exist before a v0 transaction can use it.",
        },
        "instructions": {
            "count": instructions.len(),
            "dataBytes": instruction_data_bytes,
            "items": instructions
                .iter()
                .enumerate()
                .map(|(index, instruction)| json!({
                    "index": index,
                    "programId": instruction.program_id.to_string(),
                    "accountCount": instruction.accounts.len(),
                    "dataBytes": instruction.data.len(),
                }))
                .collect::<Vec<_>>(),
        },
    }))
}

fn versioned_transaction_report(transaction: &VersionedTransaction) -> Value {
    let message_bytes = transaction.message.serialize().len() as u64;
    let transaction_bytes = versioned_transaction_size_bytes(transaction).ok();
    json!({
        "messageBytes": message_bytes,
        "transactionBytes": transaction_bytes,
        "fitsPacket": message_bytes <= MAX_TRANSACTION_PACKET_BYTES
            && transaction_bytes.is_some_and(|bytes| bytes <= MAX_TRANSACTION_PACKET_BYTES),
    })
}

fn oversized_dry_run_report(transaction_report: Value, payer: Pubkey) -> Value {
    let legacy_message_bytes = transaction_report["legacy"]["messageBytes"]
        .as_u64()
        .unwrap_or_default();
    let v0_without_lookup_bytes = transaction_report["v0WithoutLookupTables"]["messageBytes"]
        .as_u64()
        .unwrap_or_default();
    let hypothetical_v0_fits = transaction_report["v0WithHypotheticalLookupTable"]["report"]
        ["fitsPacket"]
        .as_bool()
        .unwrap_or(false);
    let instruction_data_bytes = transaction_report["instructions"]["dataBytes"]
        .as_u64()
        .unwrap_or_default();

    json!({
        "transaction": transaction_report,
        "fee": {
            "lamports": null,
            "skippedReason": "packed transaction exceeds Solana's packet/message size limit before fee estimation",
        },
        "rent": {
            "lamports": null,
            "accounts": [],
            "skippedReason": "simulation is unavailable because the packed transaction is too large to encode for RPC",
        },
        "total": {
            "estimatedLamports": null,
        },
        "payer": {
            "pubkey": payer.to_string(),
            "balanceLamports": null,
            "hasEnoughForEstimate": null,
            "estimatedRemainingLamports": null,
        },
        "simulation": {
            "attempted": false,
            "ok": false,
            "error": "packed transaction is too large to simulate",
            "legacyMessageBytes": legacy_message_bytes,
            "v0WithoutLookupTablesMessageBytes": v0_without_lookup_bytes,
            "maxPacketBytes": MAX_TRANSACTION_PACKET_BYTES,
            "instructionDataBytes": instruction_data_bytes,
            "v0AddressLookupTablesWouldHelp": hypothetical_v0_fits,
            "resolutionOptions": resolution_options(hypothetical_v0_fits, instruction_data_bytes),
        },
    })
}

fn resolution_options(
    hypothetical_v0_fits: bool,
    instruction_data_bytes: u64,
) -> Vec<&'static str> {
    let mut options = Vec::new();
    if hypothetical_v0_fits {
        options.push(
            "Use a v0 transaction with a pre-created on-chain Address Lookup Table containing the policy accounts. The lookup table must already exist and be active before this single transaction.",
        );
    } else {
        options.push(
            "A v0 transaction without lookup tables does not solve this size error. The current payload is too large before RPC fee estimation can run.",
        );
    }
    if instruction_data_bytes > MAX_TRANSACTION_PACKET_BYTES {
        options.push(
            "Reduce the policy payload itself: fewer allowed mints/markets, a narrower risk profile/universe, or split the policy into smaller action accounts.",
        );
    }
    options.push(
        "Split setup into multiple transactions: create the smart account first, then create one or more smaller policy actions.",
    );
    options
}

fn signed_v0_transaction(
    payer: &Pubkey,
    instructions: &[Instruction],
    address_lookup_table_accounts: &[AddressLookupTableAccount],
    blockhash: solana_sdk::hash::Hash,
    signer: &impl Signer,
) -> Result<VersionedTransaction, Box<dyn std::error::Error>> {
    let message = v0::Message::try_compile(
        payer,
        instructions,
        address_lookup_table_accounts,
        blockhash,
    )?;
    Ok(VersionedTransaction::try_new(
        VersionedMessage::V0(message),
        &[signer],
    )?)
}

fn hypothetical_lookup_table_account(
    payer: &Pubkey,
    instructions: &[Instruction],
) -> AddressLookupTableAccount {
    let mut addresses = Vec::new();
    for instruction in instructions {
        push_unique_lookup_address(&mut addresses, payer, instruction.program_id);
        for account in &instruction.accounts {
            if !account.is_signer {
                push_unique_lookup_address(&mut addresses, payer, account.pubkey);
            }
        }
    }

    AddressLookupTableAccount {
        key: Pubkey::new_unique(),
        addresses,
    }
}

fn push_unique_lookup_address(addresses: &mut Vec<Pubkey>, payer: &Pubkey, address: Pubkey) {
    if address != *payer && !addresses.contains(&address) {
        addresses.push(address);
    }
}

fn derive_squads_settings(seed: u128) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_SETTINGS,
            &seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

fn derive_squads_vault(settings: &Pubkey, vault_index: u8) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            settings.as_ref(),
            SQUADS_SEED_SMART_ACCOUNT,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

fn derive_squads_program_config() -> Pubkey {
    Pubkey::find_program_address(
        &[SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

fn create_squads_smart_account_instruction(
    payer: Pubkey,
    signers: &[Pubkey],
    seed: u128,
    treasury: Pubkey,
) -> Instruction {
    assert!(seed > 0, "Squads smart-account seed starts at 1");
    let program_config = derive_squads_program_config();
    let settings = derive_squads_settings(seed).0;

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(program_config, false),
            AccountMeta::new(treasury, false),
            AccountMeta::new(payer, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new(settings, false),
        ],
        data: serialize_squads_create_smart_account_args(signers),
    }
}

fn serialize_squads_create_smart_account_args(signers: &[Pubkey]) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR);
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    1u16.serialize(&mut data).unwrap();
    (signers.len() as u32).serialize(&mut data).unwrap();
    for signer in signers {
        signer.serialize(&mut data).unwrap();
        SQUADS_FULL_PERMISSIONS_MASK.serialize(&mut data).unwrap();
    }
    0u32.serialize(&mut data).unwrap();
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    Option::<String>::None.serialize(&mut data).unwrap();
    data
}

fn create_squads_policy_remove_probe_instruction(
    settings: Pubkey,
    signer: Pubkey,
    probe_policy_account: Pubkey,
) -> Instruction {
    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(settings, false),
            AccountMeta::new(signer, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(probe_policy_account, false),
        ],
        data: serialize_squads_policy_remove_probe_args(probe_policy_account),
    }
}

fn serialize_squads_policy_remove_probe_args(probe_policy_account: Pubkey) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_SETTINGS_TRANSACTION_SYNC_DISCRIMINATOR);
    SQUADS_SYNC_SIGNER_COUNT.serialize(&mut data).unwrap();
    1u32.serialize(&mut data).unwrap();
    SQUADS_SETTINGS_ACTION_POLICY_REMOVE_TAG
        .serialize(&mut data)
        .unwrap();
    probe_policy_account.serialize(&mut data).unwrap();
    Option::<String>::None.serialize(&mut data).unwrap();
    data
}

fn build_swap_lanes(cli: &Cli, cluster_config: ClusterConfig) -> Vec<SwapLane> {
    if cli.same_mint_only {
        return Vec::new();
    }

    let mut lanes = Vec::with_capacity(cli.swap_lanes.len());
    for lane in &cli.swap_lanes {
        match lane {
            SwapLaneArg::Jupiter => lanes.push(SwapLane::Jupiter(JupiterSwapContract {
                program_id: cluster_config.jupiter_v6_program_id,
                exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
                max_slippage_bps: cli.jupiter_max_slippage_bps,
            })),
            SwapLaneArg::LoyalHub => {
                let hub_authorizer = cli
                    .loyal_hub_authorizer
                    .unwrap_or(cluster_config.loyal_hub_authorizer);
                lanes.push(SwapLane::LoyalHub {
                    hub_authorizer,
                    max_fee_bps: cli.max_fee_bps,
                });
            }
        }
    }
    lanes
}

fn db_input(
    cli: &Cli,
    user_pubkey: Pubkey,
    router_pubkey: Pubkey,
    settings: Pubkey,
    vault: Pubkey,
    policy_action: PolicyActionPlan,
    setup: &loyal_actions::YieldRouteActionSetup,
    signature: Signature,
    slot: u64,
) -> PolicyDbInput {
    PolicyDbInput {
        signature: signature.to_string(),
        slot,
        cluster: cli.cluster.as_db_value().to_owned(),
        settings: settings.to_string(),
        authority: user_pubkey.to_string(),
        policy_seed: policy_action.seed,
        policy_account: policy_action.account.to_string(),
        vault_index: cli.vault_index,
        vault_pubkey: vault.to_string(),
        delegated_signers: vec![router_pubkey.to_string()],
        threshold: 1,
        route_modes: route_modes(&setup.spec.swap_lanes),
        stable_mints: pubkeys_to_strings(&setup.spec.universe.stable_mints),
        kamino_markets: pubkeys_to_strings(&setup.spec.universe.kamino_markets),
        kamino_liquidity_mints: pubkeys_to_strings(&setup.spec.universe.kamino_liquidity_mints),
        universe_preset: Some("kamino_stable".to_owned()),
        risk_profile: Some(cli.risk_profile.as_db_value().to_owned()),
        swap_lanes: swap_lanes_json(&setup.spec.swap_lanes),
    }
}

async fn upsert_policy_and_vault(
    pool: &PgPool,
    input: &PolicyDbInput,
) -> Result<(i64, i64), Box<dyn std::error::Error>> {
    let mut tx = pool.begin().await?;
    let policy_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.route_policies
            (cluster, settings, authority, policy_seed, policy_account, vault_index, vault_pubkey,
             delegated_signers, threshold, route_modes, stable_mints, kamino_markets, kamino_liquidity_mints,
             universe_preset, risk_profile, swap_lanes, active, last_seen_slot, last_seen_signature)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, TRUE, $17, $18)
        ON CONFLICT (cluster, policy_account) DO UPDATE SET
            settings = EXCLUDED.settings,
            authority = EXCLUDED.authority,
            policy_seed = EXCLUDED.policy_seed,
            vault_index = EXCLUDED.vault_index,
            vault_pubkey = EXCLUDED.vault_pubkey,
            delegated_signers = EXCLUDED.delegated_signers,
            threshold = EXCLUDED.threshold,
            route_modes = EXCLUDED.route_modes,
            stable_mints = EXCLUDED.stable_mints,
            kamino_markets = EXCLUDED.kamino_markets,
            kamino_liquidity_mints = EXCLUDED.kamino_liquidity_mints,
            universe_preset = EXCLUDED.universe_preset,
            risk_profile = EXCLUDED.risk_profile,
            swap_lanes = EXCLUDED.swap_lanes,
            active = TRUE,
            last_seen_at = now(),
            last_seen_slot = EXCLUDED.last_seen_slot,
            last_seen_signature = EXCLUDED.last_seen_signature
        RETURNING id
        "#,
    )
    .bind(&input.cluster)
    .bind(&input.settings)
    .bind(&input.authority)
    .bind(input.policy_seed as i64)
    .bind(&input.policy_account)
    .bind(i16::from(input.vault_index))
    .bind(&input.vault_pubkey)
    .bind(&input.delegated_signers)
    .bind(i32::from(input.threshold))
    .bind(&input.route_modes)
    .bind(&input.stable_mints)
    .bind(&input.kamino_markets)
    .bind(&input.kamino_liquidity_mints)
    .bind(input.universe_preset.as_deref())
    .bind(input.risk_profile.as_deref())
    .bind(&input.swap_lanes)
    .bind(input.slot as i64)
    .bind(&input.signature)
    .fetch_one(&mut *tx)
    .await?;

    let vault_id = sqlx::query_scalar::<_, i64>(
        r#"
        INSERT INTO loyal_yield.managed_vaults
            (cluster, settings, vault_index, vault_pubkey, active_policy_id, active)
        VALUES ($1, $2, $3, $4, $5, TRUE)
        ON CONFLICT (cluster, settings, vault_index, vault_pubkey) DO UPDATE SET
            active_policy_id = EXCLUDED.active_policy_id,
            active = TRUE,
            last_seen_at = now()
        RETURNING id
        "#,
    )
    .bind(&input.cluster)
    .bind(&input.settings)
    .bind(i16::from(input.vault_index))
    .bind(&input.vault_pubkey)
    .bind(policy_id)
    .fetch_one(&mut *tx)
    .await?;

    tx.commit().await?;
    Ok((policy_id, vault_id))
}

async fn upsert_policy_actions_and_vault(
    pool: &PgPool,
    cli: &Cli,
    user_pubkey: Pubkey,
    router_pubkey: Pubkey,
    settings: Pubkey,
    vault: Pubkey,
    setup: &loyal_actions::YieldRouteActionSetup,
    submitted_transactions: &[SubmittedTransaction],
) -> Result<Vec<(PolicyActionPlan, i64, i64)>, Box<dyn std::error::Error>> {
    if setup.spec.topology == RouteTopology::CombinedKamino {
        let rebalance = submitted_policy_transaction(submitted_transactions, "withdraw_policy")?;
        let swap = submitted_policy_transaction(submitted_transactions, "swap_policy")?;
        let rebalance_action = rebalance
            .policy_action
            .expect("submitted_policy_transaction returns policy transaction");
        let swap_action = swap
            .policy_action
            .expect("submitted_policy_transaction returns policy transaction");
        let mut input = db_input(
            cli,
            user_pubkey,
            router_pubkey,
            settings,
            vault,
            rebalance_action,
            setup,
            rebalance.signature,
            rebalance.slot,
        );
        input.swap_lanes =
            swap_lanes_json_with_policy_account(&setup.spec.swap_lanes, Some(swap_action.account));
        let (policy_id, vault_id) = upsert_policy_and_vault(pool, &input).await?;
        return Ok(vec![(rebalance_action, policy_id, vault_id)]);
    }

    let mut rows = Vec::new();
    for submitted in submitted_transactions {
        let Some(policy_action) = submitted.policy_action else {
            continue;
        };
        let input = db_input(
            cli,
            user_pubkey,
            router_pubkey,
            settings,
            vault,
            policy_action,
            setup,
            submitted.signature,
            submitted.slot,
        );
        let (policy_id, vault_id) = upsert_policy_and_vault(pool, &input).await?;
        rows.push((policy_action, policy_id, vault_id));
    }
    Ok(rows)
}

fn submitted_policy_transaction<'a>(
    submitted_transactions: &'a [SubmittedTransaction],
    label: &str,
) -> Result<&'a SubmittedTransaction, Box<dyn std::error::Error>> {
    submitted_transactions
        .iter()
        .find(|transaction| {
            transaction
                .policy_action
                .is_some_and(|action| action.label == label)
        })
        .ok_or_else(|| format!("submitted transaction missing {label}").into())
}

fn planned_output(
    cli: &Cli,
    cluster_config: ClusterConfig,
    user_pubkey: Pubkey,
    router_pubkey: Pubkey,
    settings: Pubkey,
    vault: Pubkey,
    smart_account_seed: Option<u128>,
    setup: &loyal_actions::YieldRouteActionSetup,
    policy_actions: &[PolicyActionPlan],
    submitted_transactions: Option<&[SubmittedTransaction]>,
    db_result: Option<Vec<(PolicyActionPlan, i64, i64)>>,
    dry_run_report: Option<Value>,
    policy_support_probe: Option<Value>,
) -> Value {
    let submitted_transactions_json = submitted_transactions.map(|transactions| {
        transactions
            .iter()
            .map(|transaction| {
                json!({
                    "label": transaction.label,
                    "signature": transaction.signature.to_string(),
                    "slot": transaction.slot,
                    "policyAction": transaction.policy_action.map(policy_action_json),
                })
            })
            .collect::<Vec<_>>()
    });
    let smart_account_signature = submitted_transactions.and_then(|transactions| {
        transactions
            .iter()
            .find(|transaction| transaction.label == "create_smart_account")
            .map(|transaction| transaction.signature.to_string())
    });
    let policy_signatures = submitted_transactions.map(|transactions| {
        transactions
            .iter()
            .filter_map(|transaction| {
                transaction.policy_action.map(|action| {
                    json!({
                        "label": action.label,
                        "seed": action.seed,
                        "policyAccount": action.account.to_string(),
                        "operation": action.operation.as_json_value(),
                        "signature": transaction.signature.to_string(),
                        "slot": transaction.slot,
                    })
                })
            })
            .collect::<Vec<_>>()
    });
    json!({
        "dryRun": cli.dry_run,
        "cluster": cli.cluster.as_db_value(),
        "clusterConfig": {
            "squadsSmartAccountProgramId": cluster_config.squads_smart_account_program_id.to_string(),
            "jupiterV6ProgramId": cluster_config.jupiter_v6_program_id.to_string(),
            "loyalHubSwapProgramId": cluster_config.loyal_hub_swap_program_id.to_string(),
            "loyalHubAuthorizer": cluster_config.loyal_hub_authorizer.to_string(),
            "kaminoLendProgramId": cluster_config.kamino_lend_program_id.to_string(),
        },
        "userPubkey": user_pubkey.to_string(),
        "authority": user_pubkey.to_string(),
        "delegatedSigner": router_pubkey.to_string(),
        "transactionPacking": "split_transactions",
        "smartAccountSeed": smart_account_seed.map(|seed| seed.to_string()),
        "settings": settings.to_string(),
        "vaultIndex": cli.vault_index,
        "vaultPubkey": vault.to_string(),
        "policyActions": policy_actions
            .iter()
            .copied()
            .map(policy_action_json)
            .collect::<Vec<_>>(),
        "routeModes": route_modes(&setup.spec.swap_lanes),
        "stableMints": pubkeys_to_strings(&setup.spec.universe.stable_mints),
        "kaminoMarkets": pubkeys_to_strings(&setup.spec.universe.kamino_markets),
        "kaminoLiquidityMints": pubkeys_to_strings(&setup.spec.universe.kamino_liquidity_mints),
        "universePreset": "kamino_stable",
        "riskProfile": cli.risk_profile.as_db_value(),
        "swapLanes": swap_lanes_json(&setup.spec.swap_lanes),
        "loyalHubProgramId": LOYAL_HUB_SWAP_PROGRAM_ID.to_string(),
        "transactions": submitted_transactions_json,
        "smartAccountSignature": smart_account_signature,
        "policySignatures": policy_signatures,
        "policyProgramSupport": policy_support_probe,
        "simulation": dry_run_report,
        "database": db_result.map(|rows| json!({
            "policies": rows
                .into_iter()
                .map(|(action, policy_id, vault_id)| json!({
                    "policyAction": policy_action_json(action),
                    "routePolicyId": policy_id,
                    "managedVaultId": vault_id,
                }))
                .collect::<Vec<_>>(),
        })),
    })
}

fn route_modes(swap_lanes: &[SwapLane]) -> Vec<String> {
    let mut modes = vec!["same_mint".to_owned()];
    for lane in swap_lanes {
        match lane {
            SwapLane::Jupiter(_) => modes.push("cross_mint_jupiter".to_owned()),
            SwapLane::LoyalHub { .. } => modes.push("cross_mint_loyal_hub".to_owned()),
        }
    }
    modes
}

fn swap_lanes_json(swap_lanes: &[SwapLane]) -> Value {
    swap_lanes_json_with_policy_account(swap_lanes, None)
}

fn swap_lanes_json_with_policy_account(
    swap_lanes: &[SwapLane],
    policy_account: Option<Pubkey>,
) -> Value {
    let lanes = swap_lanes
        .iter()
        .enumerate()
        .map(|(index, lane)| {
            let mut value = match lane {
                SwapLane::Jupiter(contract) => json!({
                    "kind": "jupiter",
                    "program_id": contract.program_id.to_string(),
                    "exact_in_discriminator": contract.exact_in_discriminator,
                    "max_slippage_bps": contract.max_slippage_bps,
                }),
                SwapLane::LoyalHub {
                    hub_authorizer,
                    max_fee_bps,
                } => json!({
                    "kind": "loyal_hub",
                    "program_id": LOYAL_HUB_SWAP_PROGRAM_ID.to_string(),
                    "hub_authorizer": hub_authorizer.to_string(),
                    "max_fee_bps": max_fee_bps,
                }),
            };
            if let (Some(account), Some(object)) = (policy_account, value.as_object_mut()) {
                object.insert("policy_account".to_owned(), json!(account.to_string()));
                object.insert("constraint_index".to_owned(), json!(index));
            }
            value
        })
        .collect::<Vec<_>>();
    json!(lanes)
}

fn pubkeys_to_strings(pubkeys: &[Pubkey]) -> Vec<String> {
    pubkeys.iter().map(ToString::to_string).collect()
}

fn print_json(value: Value) -> Result<(), Box<dyn std::error::Error>> {
    serde_json::to_writer_pretty(std::io::stdout(), &value)?;
    println!();
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn encoded_hex(bytes: &[u8]) -> String {
        bytes.iter().map(|byte| format!("{byte:02x}")).collect()
    }

    fn assert_parses_keypair(encoded: &str, expected: &[u8; SOLANA_KEYPAIR_LENGTH]) {
        let parsed = keypair_from_secret_string("TEST_KEYPAIR", encoded).unwrap();
        assert_eq!(parsed.to_bytes(), *expected);
    }

    #[test]
    fn keypair_secret_parser_accepts_common_64_byte_encodings() {
        let keypair = Keypair::new_from_array([7; SOLANA_SECRET_KEY_LENGTH]);
        let bytes = keypair.to_bytes();

        assert_parses_keypair(&encoded_hex(&bytes), &bytes);
        assert_parses_keypair(&bs58::encode(bytes).into_string(), &bytes);
        assert_parses_keypair(&BASE64_STANDARD.encode(bytes), &bytes);
        assert_parses_keypair(&serde_json::to_string(&bytes.to_vec()).unwrap(), &bytes);
    }

    #[test]
    fn keypair_secret_parser_accepts_32_byte_seed_hex() {
        let parsed = keypair_from_secret_string(
            "TEST_KEYPAIR",
            &encoded_hex(&[9; SOLANA_SECRET_KEY_LENGTH]),
        )
        .unwrap();

        assert_eq!(
            parsed.pubkey(),
            Keypair::new_from_array([9; SOLANA_SECRET_KEY_LENGTH]).pubkey()
        );
    }

    #[test]
    fn split_swap_policy_metadata_is_recorded_in_lane_json() {
        let swap_policy = Pubkey::new_unique();
        let lane = SwapLane::Jupiter(JupiterSwapContract {
            program_id: Pubkey::new_unique(),
            exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
            max_slippage_bps: 100,
        });

        let lanes = swap_lanes_json_with_policy_account(&[lane], Some(swap_policy));

        assert_eq!(lanes[0]["policy_account"], swap_policy.to_string());
        assert_eq!(lanes[0]["constraint_index"], 0);
    }
}
