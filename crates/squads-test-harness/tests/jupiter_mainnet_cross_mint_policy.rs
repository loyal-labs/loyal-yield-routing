use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use borsh::BorshDeserialize;
use loyal_actions::{
    compile_squads_inner_instruction, create_jupiter_cross_mint_policy_set,
    derive_associated_token_account, detect_jupiter_cross_mint_policy_account,
    earn_stablecoin_pairs, earn_stablecoins, execute_program_interaction_policy_instruction,
    execute_sync_transaction_instruction,
    jupiter::{
        parse_and_validate_jupiter_exact_in_build, JupiterBuildLimits, JupiterCrossMintPolicySeeds,
        JupiterCrossMintSourceShard, JupiterExactInBuildExpectation, JupiterLookupTableSnapshot,
        JupiterMintSnapshot, JupiterTokenAccountSnapshot, JupiterV2Dialect,
        SOLANA_MAX_COMPUTE_UNITS, SOLANA_PACKET_DATA_SIZE,
    },
    remove_policy_instruction, EarnStablecoin, EarnStablecoinPair, LoyalActionContext,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use loyal_solana_env::{
    rpc_safety::{redacted_external_error, validate_rpc_genesis_hash},
    solana_testing_keypair_from_env,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcSendTransactionConfig, RpcSimulateTransactionConfig, RpcTransactionConfig},
};
#[allow(deprecated)]
use solana_sdk::address_lookup_table::{
    program as address_lookup_table_program, state::AddressLookupTable,
};
use solana_sdk::{
    account::Account,
    commitment_config::{CommitmentConfig, CommitmentLevel},
    compute_budget::ComputeBudgetInstruction,
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0, AddressLookupTableAccount, Message, VersionedMessage},
    program_pack::Pack,
    pubkey::Pubkey,
    signature::{Keypair, Signature, Signer},
    transaction::VersionedTransaction,
};
use solana_system_interface::program as system_program;
use solana_transaction_status_client_types::{
    option_serializer::OptionSerializer, UiTransactionEncoding, UiTransactionStatusMeta,
    UiTransactionTokenBalance,
};
use spl_token::state::{Account as SplTokenAccount, Mint as SplMint};
use spl_token_2022::{
    extension::StateWithExtensions,
    state::{Account as Token2022Account, Mint as Token2022Mint},
};
use squads_test_harness::{
    derive_squads_program_config, derive_squads_settings, derive_squads_vault,
    serialize_squads_create_smart_account_args, SQUADS_PROGRAM_CONFIG_DISCRIMINATOR,
};
use std::{
    collections::BTreeMap,
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    str::FromStr,
    thread::sleep,
    time::Duration,
};

const LIVE_GATE: &str = "CROSS_MINT_MAINNET_POLICY_EXECUTE";
const CONFIRM_MAINNET_ENV: &str = "CONFIRM_MAINNET";
const PAIR_LIMIT_ENV: &str = "CROSS_MINT_MAINNET_PAIR_LIMIT";
const STATE_FILE_ENV: &str = "CROSS_MINT_MAINNET_STATE_FILE";
const DEFAULT_STATE_FILE: &str = ".agents/cross-mint-mainnet-generalized-policy-set.json";
const DEFAULT_BUILD_URL: &str = "https://api.jup.ag/swap/v2/build";
const VAULT_INDEX: u8 = 0;
const CLASSIC_POLICY_SEED: u64 = 1;
const TOKEN_2022_POLICY_SEED: u64 = 2;
const FUNDING_AMOUNT_RAW: u64 = 100_000;
const INPUT_AMOUNT_RAW: u64 = 10_000;
const MAXIMUM_SLIPPAGE_BPS: u16 = 50;
// One whole six-decimal stablecoin per source mint is ample for this low-value
// matrix while keeping the live policy's loss bound intentionally small.
const DAILY_SOURCE_MINT_SPENDING_CAP_RAW: u64 = 1_000_000;

#[allow(dead_code)]
#[derive(BorshDeserialize)]
struct ProgramConfigWire {
    discriminator: [u8; 8],
    smart_account_index: u128,
    authority: Pubkey,
    smart_account_creation_fee: u64,
    treasury: Pubkey,
    reserved: [u8; 64],
}

#[derive(BorshDeserialize)]
struct SettingsSignerWire {
    key: Pubkey,
    permissions: PermissionsWire,
}

#[derive(BorshDeserialize)]
struct PermissionsWire {
    mask: u8,
}

#[allow(dead_code)]
#[derive(BorshDeserialize)]
struct SettingsWire {
    discriminator: [u8; 8],
    seed: u128,
    settings_authority: Pubkey,
    threshold: u16,
    time_lock: u32,
    transaction_index: u64,
    stale_transaction_index: u64,
    archival_authority: Option<Pubkey>,
    archivable_after: u64,
    bump: u8,
    signers: Vec<SettingsSignerWire>,
    account_utilization: u8,
    policy_seed: Option<u64>,
    reserved2: u8,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct BuildEnvelope {
    input_mint: String,
    output_mint: String,
    in_amount: String,
    other_amount_threshold: String,
    slippage_bps: u16,
    addresses_by_lookup_table_address: BTreeMap<String, Vec<String>>,
}

#[derive(Clone)]
struct AssetSnapshot {
    asset: EarnStablecoin,
    mint: Account,
    token: Account,
    token_address: Pubkey,
}

struct PreparedSwap {
    pair: EarnStablecoinPair,
    input: AssetSnapshot,
    output: AssetSnapshot,
    minimum_output_amount: u64,
    validated: loyal_actions::jupiter::ValidatedJupiterExactInBuild,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SmartAccountPlan {
    account_index: String,
    settings: String,
    vault: String,
    program_config: String,
    treasury: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct PendingTransaction {
    stage: String,
    signature: String,
    recent_blockhash: String,
    last_valid_block_height: u64,
    signed_wire_base64: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct FinalizedTransactionEvidence {
    signature: String,
    finalized_slot: u64,
    signed_wire_sha256: String,
    packet_bytes: usize,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ImmutablePolicyReadback {
    settings: String,
    policy_account: String,
    policy_seed: u64,
    source_shard: String,
    max_slippage_bps: u16,
    daily_source_mint_spending_cap: u64,
    dialect_constraint_indexes: BTreeMap<String, u8>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ImmutablePolicyEvidence {
    source_shard: String,
    policy_seed: u64,
    policy_account: String,
    transaction: FinalizedTransactionEvidence,
    readback: ImmutablePolicyReadback,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct PairEvidence {
    input_symbol: String,
    output_symbol: String,
    input_mint: String,
    output_mint: String,
    input_amount_raw: u64,
    minimum_output_amount_raw: u64,
    finalized_source_debit_raw: u64,
    finalized_target_credit_raw: u64,
    dialect: String,
    route_step_count: usize,
    unique_account_count: usize,
    wrapped_packet_bytes: usize,
    simulated_units_consumed: u64,
    policy_account: String,
    policy_signature: String,
    swap: FinalizedTransactionEvidence,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RoutePolicyEvidence {
    policy_account: String,
    policy_seed: u64,
    transaction: FinalizedTransactionEvidence,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RouteLegAnchor {
    token_amount_raw: u64,
    obligation: String,
    obligation_exists: bool,
    deposited_collateral_amount_raw: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    minimum_deposit_amount_raw: Option<u64>,
    finalized_context_slot: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RouteLegEvidence {
    requested_amount_raw: u64,
    finalized_token_delta_raw: i128,
    before: RouteLegAnchor,
    after: RouteLegAnchor,
    simulated_units_consumed: u64,
    transaction: FinalizedTransactionEvidence,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RouteLegProgress {
    before: Option<RouteLegAnchor>,
    evidence: Option<RouteLegEvidence>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RouteSwapPlan {
    input_amount_raw: u64,
    minimum_output_amount_raw: u64,
    dialect: String,
    route_step_count: usize,
    unique_account_count: usize,
    policy_account: String,
    constraint_index: u8,
    policy_signature: String,
    policy_finalized_slot: u64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct HistoricalRouteProgress {
    input_symbol: String,
    output_symbol: String,
    history_evidence: String,
    source_reserve: String,
    target_reserve: String,
    source_deposit: RouteLegProgress,
    source_withdraw: RouteLegProgress,
    swap_plan: Option<RouteSwapPlan>,
    swap: Option<PairEvidence>,
    target_deposit: RouteLegProgress,
    target_cleanup_withdraw: RouteLegProgress,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
struct MainnetState {
    version: u8,
    wallet: String,
    smart_account: Option<SmartAccountPlan>,
    smart_account_creation: Option<FinalizedTransactionEvidence>,
    funded_mints: BTreeMap<String, FinalizedTransactionEvidence>,
    #[serde(default)]
    cross_mint_policies: BTreeMap<String, ImmutablePolicyEvidence>,
    pairs: BTreeMap<String, PairEvidence>,
    cleanup: BTreeMap<String, FinalizedTransactionEvidence>,
    #[serde(default)]
    route_policies: BTreeMap<String, RoutePolicyEvidence>,
    #[serde(default)]
    route_setup: BTreeMap<String, FinalizedTransactionEvidence>,
    #[serde(default)]
    route_auxiliary_accounts: BTreeMap<String, String>,
    #[serde(default)]
    route_auxiliary_cleanup: BTreeMap<String, String>,
    #[serde(default)]
    historical_routes: BTreeMap<String, HistoricalRouteProgress>,
    pending: Option<PendingTransaction>,
}

impl MainnetState {
    fn new(wallet: Pubkey) -> Self {
        Self {
            version: 2,
            wallet: wallet.to_string(),
            smart_account: None,
            smart_account_creation: None,
            funded_mints: BTreeMap::new(),
            cross_mint_policies: BTreeMap::new(),
            pairs: BTreeMap::new(),
            cleanup: BTreeMap::new(),
            route_policies: BTreeMap::new(),
            route_setup: BTreeMap::new(),
            route_auxiliary_accounts: BTreeMap::new(),
            route_auxiliary_cleanup: BTreeMap::new(),
            historical_routes: BTreeMap::new(),
            pending: None,
        }
    }
}

fn latest_finalized_policy_dependency_slot(state: &MainnetState) -> Option<u64> {
    state
        .smart_account_creation
        .iter()
        .map(|evidence| evidence.finalized_slot)
        .chain(
            state
                .route_policies
                .values()
                .map(|policy| policy.transaction.finalized_slot),
        )
        .chain(
            state
                .cross_mint_policies
                .values()
                .map(|policy| policy.transaction.finalized_slot),
        )
        .max()
}

#[test]
#[ignore = "mutates mainnet; requires the explicit generalized-policy wrapper"]
fn generalized_policy_set_executes_and_reconciles_on_mainnet() {
    if env::var(LIVE_GATE).ok().as_deref() != Some("1") {
        panic!("{LIVE_GATE}=1 is required; run the explicit mainnet policy wrapper");
    }
    if env::var(CONFIRM_MAINNET_ENV).ok().as_deref() != Some("1") {
        panic!("mutating mainnet verifier requires {CONFIRM_MAINNET_ENV}=1");
    }
    if let Err(error) = run_mainnet_policy_matrix() {
        panic!("{}", redacted_external_error(&error.to_string()));
    }
}

fn run_mainnet_policy_matrix() -> Result<(), Box<dyn Error>> {
    let wallet = solana_testing_keypair_from_env()?;
    let rpc_url = env::var("SOLANA_RPC_URL")?;
    let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::finalized());
    validate_rpc_genesis_hash("mainnet-beta", rpc.get_genesis_hash()?)?;
    if rpc.get_balance(&wallet.pubkey())? < 50_000_000 {
        return Err("test wallet needs at least 0.05 SOL for the resumable matrix".into());
    }
    let state_path = env::var(STATE_FILE_ENV).map_or_else(
        |_| {
            PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("../..")
                .join(DEFAULT_STATE_FILE)
        },
        PathBuf::from,
    );
    let mut state = load_state(&state_path, wallet.pubkey())?;
    let pairs = earn_stablecoin_pairs();
    let pair_limit = env::var(PAIR_LIMIT_ENV)
        .ok()
        .map(|value| value.parse::<usize>())
        .transpose()?
        .unwrap_or(pairs.len());
    if !(1..=pairs.len()).contains(&pair_limit) {
        return Err(format!("{PAIR_LIMIT_ENV} must be in 1..={}", pairs.len()).into());
    }
    let smart_account = ensure_smart_account(&rpc, &wallet, &state_path, &mut state)?;
    let settings = Pubkey::from_str(&smart_account.settings)?;
    let vault = Pubkey::from_str(&smart_account.vault)?;
    if pair_limit == pairs.len() && state.pairs.len() == pairs.len() {
        cleanup_vault(&rpc, &wallet, settings, vault, &state_path, &mut state)?;
        if !immutable_cleanup_complete(&rpc, vault, &state)? {
            return Err("completed generalized policy run did not finish cleanup".into());
        }
        eprintln!(
            "mainnet_cross_mint_generalized_policy_set progress={}/{} settings={} vault={} cleanup=true verdict=PASS",
            state.pairs.len(),
            pairs.len(),
            settings,
            vault,
        );
        return Ok(());
    }
    ensure_vault_funding(&rpc, &wallet, vault, &state_path, &mut state)?;
    ensure_cross_mint_policy_set(
        &rpc,
        &wallet,
        Pubkey::from_str(&smart_account.settings)?,
        vault,
        JupiterCrossMintPolicySeeds {
            classic: CLASSIC_POLICY_SEED,
            token_2022: TOKEN_2022_POLICY_SEED,
        },
        &state_path,
        &mut state,
    )?;
    let http = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()?;

    for pair in pairs.iter().copied().take(pair_limit) {
        let pair_key = pair_key(pair)?;
        if state.pairs.contains_key(&pair_key) {
            continue;
        }
        let prepared = prepare_swap(&rpc, &http, vault, pair, INPUT_AMOUNT_RAW)?;
        let (policy_account, constraint_index, policy) =
            select_cross_mint_policy(&state, prepared.pair, prepared.validated.dialect)?;
        let (swap_transaction, blockhash, last_valid_block_height, packet_bytes) =
            build_wrapped_swap_transaction(
                &rpc,
                &wallet,
                policy_account,
                constraint_index,
                &prepared,
            )?;
        let simulation = simulate_signed_transaction_unless_pending(
            &rpc,
            &state,
            &swap_transaction,
            &pair_key,
            Some(policy.transaction.finalized_slot),
        )?;
        let stage = format!("swap:{pair_key}");
        let swap_evidence = send_and_load_finalized(
            &rpc,
            &state_path,
            &mut state,
            &stage,
            swap_transaction,
            blockhash,
            last_valid_block_height,
        )?;
        let (source_debit, target_credit) = finalized_pair_deltas(
            &rpc,
            &swap_evidence,
            vault,
            prepared.input.asset.mint,
            prepared.output.asset.mint,
        )?;
        if source_debit != INPUT_AMOUNT_RAW
            || target_credit < prepared.minimum_output_amount
            || target_credit == 0
        {
            return Err(format!(
                "{pair_key} finalized deltas violate ExactIn: debit={source_debit} credit={target_credit} threshold={}",
                prepared.minimum_output_amount
            )
            .into());
        }
        state.pairs.insert(
            pair_key.clone(),
            PairEvidence {
                input_symbol: prepared.input.asset.symbol.to_owned(),
                output_symbol: prepared.output.asset.symbol.to_owned(),
                input_mint: prepared.input.asset.mint.to_string(),
                output_mint: prepared.output.asset.mint.to_string(),
                input_amount_raw: INPUT_AMOUNT_RAW,
                minimum_output_amount_raw: prepared.minimum_output_amount,
                finalized_source_debit_raw: source_debit,
                finalized_target_credit_raw: target_credit,
                dialect: dialect_name(prepared.validated.dialect).to_owned(),
                route_step_count: prepared.validated.route_step_count,
                unique_account_count: prepared.validated.structure.unique_account_count,
                policy_account: policy.policy_account.clone(),
                wrapped_packet_bytes: packet_bytes,
                simulated_units_consumed: simulation,
                policy_signature: policy.transaction.signature.clone(),
                swap: swap_evidence.clone(),
            },
        );
        state.pending = None;
        save_state(&state_path, &state)?;
        eprintln!(
            "mainnet_pair={pair_key} policy={} swap={} slot={} debit={} credit={} simulation_units={} packet_bytes={} verdict=PASS",
            policy.transaction.signature,
            swap_evidence.signature,
            swap_evidence.finalized_slot,
            source_debit,
            target_credit,
            simulation,
            packet_bytes,
        );
        sleep(Duration::from_millis(400));
    }

    if pair_limit == pairs.len() && state.pairs.len() == pairs.len() {
        cleanup_vault(&rpc, &wallet, settings, vault, &state_path, &mut state)?;
    }
    eprintln!(
            "mainnet_cross_mint_generalized_policy_set progress={}/{} settings={} vault={} finalized_swaps={} cleanup={} verdict=PASS",
        state.pairs.len(),
        pairs.len(),
        settings,
        vault,
        state.pairs.len(),
        pair_limit == pairs.len() && state.pairs.len() == pairs.len(),
    );
    Ok(())
}

fn load_state(path: &Path, wallet: Pubkey) -> Result<MainnetState, Box<dyn Error>> {
    let state = if path.exists() {
        serde_json::from_slice::<MainnetState>(&fs::read(path)?)?
    } else {
        MainnetState::new(wallet)
    };
    if state.version != 2 || state.wallet != wallet.to_string() {
        return Err(
            "mainnet state is not the immutable generalized-policy schema (old exact-policy state is incompatible)"
                .into(),
        );
    }
    Ok(state)
}

fn save_state(path: &Path, state: &MainnetState) -> Result<(), Box<dyn Error>> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let temporary = path.with_extension("json.tmp");
    fs::write(&temporary, serde_json::to_vec_pretty(state)?)?;
    fs::rename(temporary, path)?;
    Ok(())
}

fn immutable_cleanup_complete(
    rpc: &RpcClient,
    vault: Pubkey,
    state: &MainnetState,
) -> Result<bool, Box<dyn Error>> {
    for asset in earn_stablecoins().iter().copied() {
        let ata = derive_associated_token_account(vault, asset.mint, asset.token_program);
        if rpc
            .get_account_with_commitment(&ata, CommitmentConfig::finalized())?
            .value
            .is_some()
        {
            return Ok(false);
        }
    }
    for evidence in state.cross_mint_policies.values() {
        let policy = Pubkey::from_str(&evidence.policy_account)?;
        if rpc
            .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
            .value
            .is_some()
        {
            return Ok(false);
        }
    }
    Ok(state.cross_mint_policies.len() == 2)
}

fn ensure_smart_account(
    rpc: &RpcClient,
    wallet: &Keypair,
    state_path: &Path,
    state: &mut MainnetState,
) -> Result<SmartAccountPlan, Box<dyn Error>> {
    let plan = match state.smart_account.clone() {
        Some(plan) => plan,
        None => {
            let program_config = derive_squads_program_config();
            let account = finalized_account(rpc, program_config)?;
            if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
                return Err("Squads ProgramConfig has an unexpected owner".into());
            }
            let config = ProgramConfigWire::try_from_slice(&account.data)?;
            if config.discriminator != SQUADS_PROGRAM_CONFIG_DISCRIMINATOR
                || config.treasury == Pubkey::default()
            {
                return Err("Squads ProgramConfig bytes failed the mainnet identity check".into());
            }
            let account_index = config
                .smart_account_index
                .checked_add(1)
                .ok_or("Squads smart-account index overflow")?;
            let settings = derive_squads_settings(account_index).0;
            if rpc.get_account(&settings).is_ok() {
                return Err("next Squads Settings PDA already exists".into());
            }
            let vault = derive_squads_vault(&settings, VAULT_INDEX).0;
            let plan = SmartAccountPlan {
                account_index: account_index.to_string(),
                settings: settings.to_string(),
                vault: vault.to_string(),
                program_config: program_config.to_string(),
                treasury: config.treasury.to_string(),
            };
            state.smart_account = Some(plan.clone());
            save_state(state_path, state)?;
            plan
        }
    };
    let settings = Pubkey::from_str(&plan.settings)?;
    if state.smart_account_creation.is_none() {
        let instruction = create_mainnet_smart_account_instruction(
            wallet.pubkey(),
            Pubkey::from_str(&plan.treasury)?,
            settings,
        );
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[instruction])?;
        simulate_signed_transaction_unless_pending(
            rpc,
            state,
            &transaction,
            "create-smart-account",
            None,
        )?;
        let evidence = send_and_load_finalized(
            rpc,
            state_path,
            state,
            "create-smart-account",
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        state.smart_account_creation = Some(evidence);
        state.pending = None;
        save_state(state_path, state)?;
    }
    verify_settings_account(rpc, &plan, wallet.pubkey())?;
    Ok(plan)
}

fn create_mainnet_smart_account_instruction(
    creator: Pubkey,
    treasury: Pubkey,
    settings: Pubkey,
) -> Instruction {
    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_squads_program_config(), false),
            AccountMeta::new(treasury, false),
            AccountMeta::new(creator, true),
            AccountMeta::new_readonly(system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new(settings, false),
        ],
        data: serialize_squads_create_smart_account_args(creator),
    }
}

fn verify_settings_account(
    rpc: &RpcClient,
    plan: &SmartAccountPlan,
    expected_signer: Pubkey,
) -> Result<(), Box<dyn Error>> {
    let settings = Pubkey::from_str(&plan.settings)?;
    let account = finalized_account(rpc, settings)?;
    if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return Err("created Settings account has the wrong owner".into());
    }
    let decoded = SettingsWire::try_from_slice(&account.data)?;
    let expected_seed = plan.account_index.parse::<u128>()?;
    let (expected_settings, expected_bump) = derive_squads_settings(expected_seed);
    if decoded.discriminator != anchor_account_discriminator("Settings")
        || decoded.seed != expected_seed
        || expected_settings != settings
        || decoded.bump != expected_bump
        || decoded.settings_authority != Pubkey::default()
        || decoded.threshold != 1
        || decoded.time_lock != 0
        || decoded.stale_transaction_index > decoded.transaction_index
        || decoded.signers.len() != 1
        || decoded.signers[0].key != expected_signer
        || decoded.signers[0].permissions.mask != 7
    {
        return Err("created Settings account differs from the exact one-signer plan".into());
    }
    Ok(())
}

fn ensure_vault_funding(
    rpc: &RpcClient,
    wallet: &Keypair,
    vault: Pubkey,
    state_path: &Path,
    state: &mut MainnetState,
) -> Result<(), Box<dyn Error>> {
    for asset in earn_stablecoins().iter().copied() {
        if state.funded_mints.contains_key(asset.symbol) {
            continue;
        }
        let wallet_ata =
            derive_associated_token_account(wallet.pubkey(), asset.mint, asset.token_program);
        let vault_ata = derive_associated_token_account(vault, asset.mint, asset.token_program);
        let wallet_account = finalized_account(rpc, wallet_ata)?;
        if token_amount(&wallet_account, asset.token_program)? < FUNDING_AMOUNT_RAW {
            return Err(format!("wallet {} balance is below test funding", asset.symbol).into());
        }
        let mint_account = finalized_account(rpc, asset.mint)?;
        let decimals = mint_decimals(&mint_account, asset.token_program)?;
        let create_ata =
            spl_associated_token_account::instruction::create_associated_token_account_idempotent(
                &wallet.pubkey(),
                &vault,
                &asset.mint,
                &asset.token_program,
            );
        let transfer = transfer_checked_instruction(
            asset.token_program,
            wallet_ata,
            asset.mint,
            vault_ata,
            wallet.pubkey(),
            FUNDING_AMOUNT_RAW,
            decimals,
        )?;
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[create_ata, transfer])?;
        let stage = format!("fund:{}", asset.symbol);
        simulate_signed_transaction_unless_pending(
            rpc,
            state,
            &transaction,
            &stage,
            latest_finalized_policy_dependency_slot(state),
        )?;
        let evidence = send_and_load_finalized(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        let vault_account = finalized_account(rpc, vault_ata)?;
        if token_amount(&vault_account, asset.token_program)? < FUNDING_AMOUNT_RAW {
            return Err(format!("vault {} funding did not finalize", asset.symbol).into());
        }
        state.funded_mints.insert(asset.symbol.to_owned(), evidence);
        state.pending = None;
        save_state(state_path, state)?;
    }
    Ok(())
}

fn prepare_swap(
    rpc: &RpcClient,
    http: &reqwest::blocking::Client,
    vault: Pubkey,
    pair: EarnStablecoinPair,
    amount: u64,
) -> Result<PreparedSwap, Box<dyn Error>> {
    let assets = load_asset_snapshots(rpc, vault)?;
    let input = assets
        .get(&pair.input_mint)
        .cloned()
        .ok_or("canonical input snapshot missing")?;
    let output = assets
        .get(&pair.output_mint)
        .cloned()
        .ok_or("canonical output snapshot missing")?;
    if token_amount(&input.token, input.asset.token_program)? < amount {
        return Err(format!("{} vault balance is below swap amount", input.asset.symbol).into());
    }
    let build_bytes = fetch_build(http, vault, input.asset, output.asset, amount)?;
    let envelope: BuildEnvelope = serde_json::from_slice(&build_bytes)?;
    if envelope.input_mint != input.asset.mint.to_string()
        || envelope.output_mint != output.asset.mint.to_string()
        || envelope.in_amount != amount.to_string()
        || envelope.slippage_bps > MAXIMUM_SLIPPAGE_BPS
    {
        return Err("Jupiter build identity differs from the requested pair".into());
    }
    let lookup_tables = finalized_lookup_tables(rpc, &envelope.addresses_by_lookup_table_address)?;
    let minimum_output_amount = envelope.other_amount_threshold.parse()?;
    let expected = JupiterExactInBuildExpectation {
        authority: vault,
        input_mint: JupiterMintSnapshot {
            address: input.asset.mint,
            owner_program: input.mint.owner,
            data: input.mint.data.clone(),
        },
        output_mint: JupiterMintSnapshot {
            address: output.asset.mint,
            owner_program: output.mint.owner,
            data: output.mint.data.clone(),
        },
        input_token_account: JupiterTokenAccountSnapshot {
            address: input.token_address,
            owner_program: input.token.owner,
            data: input.token.data.clone(),
        },
        output_token_account: JupiterTokenAccountSnapshot {
            address: output.token_address,
            owner_program: output.token.owner,
            data: output.token.data.clone(),
        },
        additional_token_accounts: assets
            .values()
            .filter(|snapshot| {
                snapshot.asset.mint != input.asset.mint && snapshot.asset.mint != output.asset.mint
            })
            .map(|snapshot| JupiterTokenAccountSnapshot {
                address: snapshot.token_address,
                owner_program: snapshot.token.owner,
                data: snapshot.token.data.clone(),
            })
            .collect(),
        input_amount: amount,
        minimum_output_amount,
        maximum_slippage_bps: MAXIMUM_SLIPPAGE_BPS,
        requested_platform_fee_bps: 0,
        lookup_tables: lookup_tables
            .iter()
            .map(|table| JupiterLookupTableSnapshot {
                address: table.key,
                addresses: table.addresses.clone(),
            })
            .collect(),
        limits: JupiterBuildLimits::default(),
    };
    let validated = parse_and_validate_jupiter_exact_in_build(&build_bytes, &expected)?;
    Ok(PreparedSwap {
        pair,
        input,
        output,
        minimum_output_amount,
        validated,
    })
}

fn ensure_cross_mint_policy_set(
    rpc: &RpcClient,
    wallet: &Keypair,
    settings: Pubkey,
    vault: Pubkey,
    seeds: JupiterCrossMintPolicySeeds,
    state_path: &Path,
    state: &mut MainnetState,
) -> Result<(), Box<dyn Error>> {
    let context = LoyalActionContext {
        settings,
        authority: wallet.pubkey(),
        delegated_signer: wallet.pubkey(),
        account_index: VAULT_INDEX,
        vault,
    };
    let policy_set = create_jupiter_cross_mint_policy_set(
        context,
        MAXIMUM_SLIPPAGE_BPS,
        DAILY_SOURCE_MINT_SPENDING_CAP_RAW,
        seeds,
    )?;
    for (label, source_shard, policy_seed, policy_account, instruction) in [
        (
            "classic",
            JupiterCrossMintSourceShard::Classic,
            seeds.classic,
            policy_set.classic.account,
            policy_set.classic.instruction.clone(),
        ),
        (
            "token2022",
            JupiterCrossMintSourceShard::Token2022,
            seeds.token_2022,
            policy_set.token_2022.account,
            policy_set.token_2022.instruction.clone(),
        ),
    ] {
        let stage = format!("policy-create:{label}");
        if let Some(evidence) = state.cross_mint_policies.get(label) {
            if Pubkey::from_str(&evidence.policy_account)? != policy_account
                || evidence.policy_seed != policy_seed
            {
                return Err(format!("recorded immutable {label} policy identity changed").into());
            }
            let readback = verify_cross_mint_policy_account(
                rpc,
                policy_account,
                settings,
                wallet.pubkey(),
                source_shard,
                policy_seed,
            )?;
            if evidence.readback != readback {
                return Err(format!("recorded immutable {label} policy readback changed").into());
            }
            continue;
        }
        if state.pending.as_ref().map(|pending| pending.stage.as_str()) != Some(stage.as_str()) {
            let settings_account = finalized_account(rpc, settings)?;
            let settings_wire = SettingsWire::try_from_slice(&settings_account.data)?;
            if settings_wire.policy_seed.unwrap_or(0).checked_add(1) != Some(policy_seed) {
                return Err(format!(
                    "immutable {label} policy seed {policy_seed} is not the next finalized Settings seed"
                )
                .into());
            }
        }
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[instruction])?;
        simulate_signed_transaction_unless_pending(
            rpc,
            state,
            &transaction,
            &stage,
            latest_finalized_policy_dependency_slot(state),
        )?;
        let transaction_evidence = send_and_load_finalized(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        let readback = verify_cross_mint_policy_account(
            rpc,
            policy_account,
            settings,
            wallet.pubkey(),
            source_shard,
            policy_seed,
        )?;
        state.cross_mint_policies.insert(
            label.to_owned(),
            ImmutablePolicyEvidence {
                source_shard: source_shard_name(source_shard).to_owned(),
                policy_seed,
                policy_account: policy_account.to_string(),
                transaction: transaction_evidence,
                readback,
            },
        );
        state.pending = None;
        save_state(state_path, state)?;
    }
    Ok(())
}

fn verify_cross_mint_policy_account(
    rpc: &RpcClient,
    policy_account: Pubkey,
    settings: Pubkey,
    delegated_signer: Pubkey,
    source_shard: JupiterCrossMintSourceShard,
    policy_seed: u64,
) -> Result<ImmutablePolicyReadback, Box<dyn Error>> {
    let account = finalized_account(rpc, policy_account)?;
    if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return Err("Jupiter generalized policy account has the wrong owner".into());
    }
    let policy = detect_jupiter_cross_mint_policy_account(&account.data)?
        .ok_or("on-chain policy is not the generalized Jupiter source-shard shape")?;
    if policy.settings != settings
        || policy.policy_seed != policy_seed
        || policy.policy_account != policy_account
        || policy.account_index != VAULT_INDEX
        || policy.vault != derive_squads_vault(&settings, VAULT_INDEX).0
        || policy.delegated_signer != delegated_signer
        || policy.threshold != 1
        || policy.source_shard != source_shard
        || policy.max_slippage_bps != MAXIMUM_SLIPPAGE_BPS
        || policy.daily_source_mint_spending_cap != DAILY_SOURCE_MINT_SPENDING_CAP_RAW
        || policy
            .dialect_constraint_indexes
            .get(&JupiterV2Dialect::RouteV2)
            != Some(&0)
        || policy
            .dialect_constraint_indexes
            .get(&JupiterV2Dialect::SharedAccountsRouteV2)
            != Some(&1)
        || policy.dialect_constraint_indexes.len() != 2
    {
        return Err(
            "finalized generalized policy readback differs from the immutable source-shard contract"
                .into(),
        );
    }
    Ok(ImmutablePolicyReadback {
        settings: policy.settings.to_string(),
        policy_account: policy.policy_account.to_string(),
        policy_seed: policy.policy_seed,
        source_shard: source_shard_name(policy.source_shard).to_owned(),
        max_slippage_bps: policy.max_slippage_bps,
        daily_source_mint_spending_cap: policy.daily_source_mint_spending_cap,
        dialect_constraint_indexes: policy
            .dialect_constraint_indexes
            .into_iter()
            .map(|(dialect, index)| (dialect_name(dialect).to_owned(), index))
            .collect(),
    })
}

fn select_cross_mint_policy(
    state: &MainnetState,
    pair: EarnStablecoinPair,
    dialect: JupiterV2Dialect,
) -> Result<(Pubkey, u8, ImmutablePolicyEvidence), Box<dyn Error>> {
    let source_shard = if JupiterCrossMintSourceShard::Classic.contains(pair.input_mint) {
        JupiterCrossMintSourceShard::Classic
    } else if JupiterCrossMintSourceShard::Token2022.contains(pair.input_mint) {
        JupiterCrossMintSourceShard::Token2022
    } else {
        return Err("cross-mint source is outside the immutable policy set".into());
    };
    let label = source_shard_name(source_shard);
    let evidence = state
        .cross_mint_policies
        .get(label)
        .cloned()
        .ok_or("immutable cross-mint policy readback is missing")?;
    let constraint_index = match dialect {
        JupiterV2Dialect::RouteV2 => 0,
        JupiterV2Dialect::SharedAccountsRouteV2 => 1,
    };
    Ok((
        Pubkey::from_str(&evidence.policy_account)?,
        constraint_index,
        evidence,
    ))
}

const fn source_shard_name(source_shard: JupiterCrossMintSourceShard) -> &'static str {
    match source_shard {
        JupiterCrossMintSourceShard::Classic => "classic",
        JupiterCrossMintSourceShard::Token2022 => "token2022",
    }
}

fn build_wrapped_swap_transaction(
    rpc: &RpcClient,
    wallet: &Keypair,
    policy_account: Pubkey,
    constraint_index: u8,
    prepared: &PreparedSwap,
) -> Result<(VersionedTransaction, Hash, u64, usize), Box<dyn Error>> {
    let mut transaction_accounts = Vec::<AccountMeta>::new();
    let compiled = compile_squads_inner_instruction(
        &mut transaction_accounts,
        prepared.validated.swap_instruction.clone(),
    );
    let wrapped = execute_program_interaction_policy_instruction(
        policy_account,
        wallet.pubkey(),
        VAULT_INDEX,
        vec![compiled],
        vec![constraint_index],
        transaction_accounts,
    );
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let message = v0::Message::try_compile(
        &wallet.pubkey(),
        &[
            ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
            ComputeBudgetInstruction::set_compute_unit_price(
                prepared.validated.compute_budget.unit_price_micro_lamports,
            ),
            wrapped,
        ],
        &prepared.validated.lookup_tables,
        blockhash,
    )?;
    let transaction = VersionedTransaction::try_new(VersionedMessage::V0(message), &[wallet])?;
    let packet_bytes = bincode::serialize(&transaction)?.len();
    if packet_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err(format!("policy-wrapped swap is {packet_bytes} bytes").into());
    }
    Ok((
        transaction,
        blockhash,
        last_valid_block_height,
        packet_bytes,
    ))
}

fn cleanup_vault(
    rpc: &RpcClient,
    wallet: &Keypair,
    settings: Pubkey,
    vault: Pubkey,
    state_path: &Path,
    state: &mut MainnetState,
) -> Result<(), Box<dyn Error>> {
    for asset in earn_stablecoins().iter().copied() {
        let stage = format!("cleanup:{}", asset.symbol);
        let vault_ata = derive_associated_token_account(vault, asset.mint, asset.token_program);
        let ata_exists = rpc
            .get_account_with_commitment(&vault_ata, CommitmentConfig::finalized())?
            .value
            .is_some();
        if state.cleanup.contains_key(&stage) {
            if ata_exists {
                return Err(
                    format!("recorded cleanup did not remove {} vault ATA", asset.symbol).into(),
                );
            }
            continue;
        }
        if !ata_exists
            && state.pending.as_ref().map(|pending| pending.stage.as_str()) != Some(stage.as_str())
        {
            continue;
        }
        if state.pending.as_ref().map(|pending| pending.stage.as_str()) == Some(stage.as_str()) {
            let pending = state
                .pending
                .as_ref()
                .ok_or("cleanup pending stage disappeared")?;
            let wire = BASE64_STANDARD.decode(&pending.signed_wire_base64)?;
            let transaction = bincode::deserialize(&wire)?;
            let evidence = send_and_load_finalized(
                rpc,
                state_path,
                state,
                &stage,
                transaction,
                Hash::from_str(&pending.recent_blockhash)?,
                pending.last_valid_block_height,
            )?;
            if rpc
                .get_account_with_commitment(&vault_ata, CommitmentConfig::finalized())?
                .value
                .is_some()
            {
                return Err(format!("{} vault ATA was not closed", asset.symbol).into());
            }
            state.cleanup.insert(stage, evidence);
            state.pending = None;
            save_state(state_path, state)?;
            continue;
        }
        let wallet_ata =
            derive_associated_token_account(wallet.pubkey(), asset.mint, asset.token_program);
        let account = finalized_account(rpc, vault_ata)?;
        let amount = token_amount(&account, asset.token_program)?;
        let mint = finalized_account(rpc, asset.mint)?;
        let decimals = mint_decimals(&mint, asset.token_program)?;
        let transfer = transfer_checked_instruction(
            asset.token_program,
            vault_ata,
            asset.mint,
            wallet_ata,
            vault,
            amount,
            decimals,
        )?;
        let close = close_token_account_instruction(
            asset.token_program,
            vault_ata,
            wallet.pubkey(),
            vault,
        )?;
        let mut accounts = Vec::new();
        let compiled_transfer = compile_squads_inner_instruction(&mut accounts, transfer);
        let compiled_close = compile_squads_inner_instruction(&mut accounts, close);
        let execute = execute_sync_transaction_instruction(
            settings,
            wallet.pubkey(),
            VAULT_INDEX,
            vec![compiled_transfer, compiled_close],
            accounts,
        );
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[execute])?;
        simulate_signed_transaction_unless_pending(rpc, state, &transaction, &stage, None)?;
        let evidence = send_and_load_finalized(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        if rpc
            .get_account_with_commitment(&vault_ata, CommitmentConfig::finalized())?
            .value
            .is_some()
        {
            return Err(format!("{} vault ATA was not closed", asset.symbol).into());
        }
        state.cleanup.insert(stage, evidence);
        state.pending = None;
        save_state(state_path, state)?;
    }
    for label in ["classic", "token2022"] {
        let stage = format!("remove-policy:{label}");
        let policy = Pubkey::from_str(
            state
                .cross_mint_policies
                .get(label)
                .ok_or("cleanup is missing an immutable cross-mint policy")?
                .policy_account
                .as_str(),
        )?;
        if state.cleanup.contains_key(&stage) {
            if rpc
                .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
                .value
                .is_some()
            {
                return Err(format!("removed {label} policy account still exists").into());
            }
            continue;
        }
        if rpc
            .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
            .value
            .is_none()
        {
            continue;
        }
        let instruction = remove_policy_instruction(settings, wallet.pubkey(), policy);
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[instruction])?;
        simulate_signed_transaction_unless_pending(rpc, state, &transaction, &stage, None)?;
        let evidence = send_and_load_finalized(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        if rpc
            .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
            .value
            .is_some()
        {
            return Err(format!(
                "removed {label} policy account still exists at finalized commitment"
            )
            .into());
        }
        state.cleanup.insert(stage, evidence);
        state.pending = None;
        save_state(state_path, state)?;
    }
    Ok(())
}

fn legacy_transaction(
    rpc: &RpcClient,
    wallet: &Keypair,
    instructions: &[Instruction],
) -> Result<(VersionedTransaction, Hash, u64), Box<dyn Error>> {
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let message = Message::new_with_blockhash(instructions, Some(&wallet.pubkey()), &blockhash);
    let transaction = VersionedTransaction::try_new(VersionedMessage::Legacy(message), &[wallet])?;
    Ok((transaction, blockhash, last_valid_block_height))
}

fn simulate_signed_transaction(
    rpc: &RpcClient,
    transaction: &VersionedTransaction,
    stage: &str,
    min_context_slot: Option<u64>,
) -> Result<u64, Box<dyn Error>> {
    let packet_bytes = bincode::serialize(transaction)?.len();
    if packet_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err(format!("{stage} transaction is {packet_bytes} bytes").into());
    }
    let mut attempt = 0;
    let simulation = loop {
        match rpc.simulate_transaction_with_config(
            transaction,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                replace_recent_blockhash: false,
                commitment: Some(CommitmentConfig::finalized()),
                min_context_slot,
                ..RpcSimulateTransactionConfig::default()
            },
        ) {
            Ok(simulation) => break simulation,
            Err(_error) if min_context_slot.is_some() && attempt < 4 => {
                attempt += 1;
                sleep(Duration::from_secs(1));
                eprintln!(
                    "{stage} simulation RPC retry at required policy slot; retry={attempt}/4"
                );
            }
            Err(error) => return Err(error.into()),
        }
    };
    if let Some(error) = simulation.value.err {
        let logs = simulation.value.logs.unwrap_or_default();
        let reverse_tail = logs
            .iter()
            .rev()
            .take(16)
            .cloned()
            .collect::<Vec<_>>()
            .join(" | ");
        return Err(format!(
            "{stage} simulation failed: {error:?}; reverse tail logs: {reverse_tail}"
        )
        .into());
    }
    Ok(simulation.value.units_consumed.unwrap_or_default())
}

fn simulate_signed_transaction_unless_pending(
    rpc: &RpcClient,
    state: &MainnetState,
    transaction: &VersionedTransaction,
    stage: &str,
    min_context_slot: Option<u64>,
) -> Result<u64, Box<dyn Error>> {
    if state.pending.as_ref().map(|pending| pending.stage.as_str()) == Some(stage) {
        return Ok(0);
    }
    simulate_signed_transaction(rpc, transaction, stage, min_context_slot)
}

#[allow(clippy::too_many_arguments)]
fn send_and_load_finalized(
    rpc: &RpcClient,
    state_path: &Path,
    state: &mut MainnetState,
    stage: &str,
    proposed: VersionedTransaction,
    proposed_blockhash: Hash,
    proposed_last_valid_block_height: u64,
) -> Result<FinalizedTransactionEvidence, Box<dyn Error>> {
    let (transaction, blockhash, last_valid_block_height, expected_signature, wire) =
        if let Some(pending) = state.pending.as_ref() {
            if pending.stage != stage {
                return Err(format!(
                    "pending stage {} must be resolved before {stage}",
                    pending.stage
                )
                .into());
            }
            let wire = BASE64_STANDARD.decode(&pending.signed_wire_base64)?;
            let transaction: VersionedTransaction = bincode::deserialize(&wire)?;
            let signature = Signature::from_str(&pending.signature)?;
            if transaction.signatures.first() != Some(&signature) {
                return Err("pending signed wire does not match its signature".into());
            }
            (
                transaction,
                Hash::from_str(&pending.recent_blockhash)?,
                pending.last_valid_block_height,
                signature,
                wire,
            )
        } else {
            let wire = bincode::serialize(&proposed)?;
            let expected_signature = *proposed
                .signatures
                .first()
                .ok_or("signed transaction has no signature")?;
            state.pending = Some(PendingTransaction {
                stage: stage.to_owned(),
                signature: expected_signature.to_string(),
                recent_blockhash: proposed_blockhash.to_string(),
                last_valid_block_height: proposed_last_valid_block_height,
                signed_wire_base64: BASE64_STANDARD.encode(&wire),
            });
            save_state(state_path, state)?;
            (
                proposed,
                proposed_blockhash,
                proposed_last_valid_block_height,
                expected_signature,
                wire,
            )
        };

    let status = rpc
        .get_signature_statuses(&[expected_signature])?
        .value
        .into_iter()
        .next()
        .flatten();
    if let Some(status) = &status {
        if let Some(error) = &status.err {
            return Err(format!("{stage} failed on chain: {error:?}").into());
        }
    }
    let finalized = status
        .as_ref()
        .is_some_and(|status| status.satisfies_commitment(CommitmentConfig::finalized()));
    if !finalized {
        if rpc.get_block_height()? > last_valid_block_height {
            return Err(format!(
                "{stage} signed transaction expired without finalized evidence; no-effect audit required"
            )
            .into());
        }
        let sent = rpc.send_transaction_with_config(
            &transaction,
            RpcSendTransactionConfig {
                skip_preflight: false,
                preflight_commitment: Some(CommitmentLevel::Finalized),
                ..RpcSendTransactionConfig::default()
            },
        )?;
        if sent != expected_signature {
            return Err("RPC returned a different transaction signature".into());
        }
        rpc.confirm_transaction_with_spinner(
            &expected_signature,
            &blockhash,
            CommitmentConfig::finalized(),
        )?;
    }
    let finalized_transaction = rpc.get_transaction_with_config(
        &expected_signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let decoded = finalized_transaction
        .transaction
        .transaction
        .decode()
        .ok_or("finalized transaction wire did not decode")?;
    if bincode::serialize(&decoded)? != wire {
        return Err("finalized signed wire differs from persisted bytes".into());
    }
    let meta = finalized_transaction
        .transaction
        .meta
        .as_ref()
        .ok_or("finalized transaction omitted metadata")?;
    if let Some(error) = &meta.err {
        return Err(format!("{stage} finalized with error: {error:?}").into());
    }
    let signed_wire_sha256 = format!("{:x}", Sha256::digest(&wire));
    Ok(FinalizedTransactionEvidence {
        signature: expected_signature.to_string(),
        finalized_slot: finalized_transaction.slot,
        signed_wire_sha256,
        packet_bytes: wire.len(),
    })
}

fn finalized_pair_deltas(
    rpc: &RpcClient,
    evidence: &FinalizedTransactionEvidence,
    vault: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> Result<(u64, u64), Box<dyn Error>> {
    let signature = Signature::from_str(&evidence.signature)?;
    let finalized = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    if finalized.slot != evidence.finalized_slot {
        return Err("finalized swap slot changed between receipt reads".into());
    }
    let meta = finalized
        .transaction
        .meta
        .as_ref()
        .ok_or("finalized swap omitted metadata")?;
    let input = token_delta(meta, vault, input_mint)?;
    let output = token_delta(meta, vault, output_mint)?;
    let debit = input
        .0
        .checked_sub(input.1)
        .ok_or("input balance increased instead of debiting")?;
    let credit = output
        .1
        .checked_sub(output.0)
        .ok_or("output balance decreased instead of crediting")?;
    Ok((debit, credit))
}

fn token_delta(
    meta: &UiTransactionStatusMeta,
    owner: Pubkey,
    mint: Pubkey,
) -> Result<(u64, u64), Box<dyn Error>> {
    let pre = option_serializer_slice(&meta.pre_token_balances);
    let post = option_serializer_slice(&meta.post_token_balances);
    let matching_pre = matching_token_balance(pre, owner, mint)?;
    let matching_post = post
        .iter()
        .find(|balance| balance.account_index == matching_pre.account_index)
        .ok_or("post-token balance is missing the matched account index")?;
    if matching_post.mint != mint.to_string()
        || option_serializer_string(&matching_post.owner) != Some(owner.to_string().as_str())
    {
        return Err("post-token balance identity differs from pre-token balance".into());
    }
    Ok((
        matching_pre.ui_token_amount.amount.parse()?,
        matching_post.ui_token_amount.amount.parse()?,
    ))
}

fn matching_token_balance(
    balances: &[UiTransactionTokenBalance],
    owner: Pubkey,
    mint: Pubkey,
) -> Result<&UiTransactionTokenBalance, Box<dyn Error>> {
    let owner = owner.to_string();
    let mint = mint.to_string();
    let matches = balances
        .iter()
        .filter(|balance| {
            balance.mint == mint && option_serializer_string(&balance.owner) == Some(owner.as_str())
        })
        .collect::<Vec<_>>();
    let [balance] = matches.as_slice() else {
        return Err("finalized metadata must identify exactly one vault token account".into());
    };
    Ok(*balance)
}

fn option_serializer_slice<T>(value: &OptionSerializer<Vec<T>>) -> &[T] {
    match value {
        OptionSerializer::Some(value) => value,
        OptionSerializer::None | OptionSerializer::Skip => &[],
    }
}

fn option_serializer_string(value: &OptionSerializer<String>) -> Option<&str> {
    match value {
        OptionSerializer::Some(value) => Some(value.as_str()),
        OptionSerializer::None | OptionSerializer::Skip => None,
    }
}

fn load_asset_snapshots(
    rpc: &RpcClient,
    authority: Pubkey,
) -> Result<BTreeMap<Pubkey, AssetSnapshot>, Box<dyn Error>> {
    earn_stablecoins()
        .iter()
        .copied()
        .map(|asset| {
            let token_address =
                derive_associated_token_account(authority, asset.mint, asset.token_program);
            Ok((
                asset.mint,
                AssetSnapshot {
                    asset,
                    mint: finalized_account(rpc, asset.mint)?,
                    token: finalized_account(rpc, token_address)?,
                    token_address,
                },
            ))
        })
        .collect()
}

fn finalized_account(rpc: &RpcClient, address: Pubkey) -> Result<Account, Box<dyn Error>> {
    rpc.get_account_with_commitment(&address, CommitmentConfig::finalized())?
        .value
        .ok_or_else(|| format!("finalized account {address} is missing").into())
}

fn finalized_lookup_tables(
    rpc: &RpcClient,
    expected: &BTreeMap<String, Vec<String>>,
) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
    expected
        .iter()
        .map(|(key, expected_addresses)| {
            let key = Pubkey::from_str(key)?;
            let account = finalized_account(rpc, key)?;
            if account.owner != address_lookup_table_program::id() {
                return Err(format!("Jupiter ALT {key} has the wrong owner").into());
            }
            let table = AddressLookupTable::deserialize(&account.data)?;
            if table.meta.deactivation_slot != u64::MAX {
                return Err(format!("Jupiter ALT {key} is deactivating").into());
            }
            let addresses = table.addresses.iter().copied().collect::<Vec<_>>();
            let expected_addresses = expected_addresses
                .iter()
                .map(|address| Pubkey::from_str(address))
                .collect::<Result<Vec<_>, _>>()?;
            if addresses != expected_addresses {
                return Err(format!("Jupiter ALT {key} differs from finalized state").into());
            }
            Ok(AddressLookupTableAccount { key, addresses })
        })
        .collect()
}

fn fetch_build(
    client: &reqwest::blocking::Client,
    authority: Pubkey,
    input: EarnStablecoin,
    output: EarnStablecoin,
    amount: u64,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let build_url =
        env::var("JUPITER_SWAP_BUILD_URL").unwrap_or_else(|_| DEFAULT_BUILD_URL.to_owned());
    let url = format!(
        "{build_url}?inputMint={}&outputMint={}&amount={amount}&taker={authority}&maxAccounts=64&slippageBps={MAXIMUM_SLIPPAGE_BPS}&onlyDirectRoutes=true&dexes=AlphaQ",
        input.mint, output.mint,
    );
    for attempt in 0..5 {
        let mut request = client.get(&url);
        if let Ok(api_key) = env::var("JUPITER_API_KEY") {
            request = request.header("x-api-key", api_key);
        }
        let response = request.send()?;
        if response.status().as_u16() == 429 && attempt < 4 {
            sleep(Duration::from_secs(attempt + 1));
            continue;
        }
        return Ok(response.error_for_status()?.bytes()?.to_vec());
    }
    Err("Jupiter retry loop exhausted".into())
}

fn token_amount(account: &Account, token_program: Pubkey) -> Result<u64, Box<dyn Error>> {
    if token_program == spl_token::id() {
        return Ok(SplTokenAccount::unpack(&account.data)?.amount);
    }
    if token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
        return Ok(
            StateWithExtensions::<Token2022Account>::unpack(&account.data)?
                .base
                .amount,
        );
    }
    Err("unsupported canonical token program".into())
}

fn mint_decimals(account: &Account, token_program: Pubkey) -> Result<u8, Box<dyn Error>> {
    if token_program == spl_token::id() {
        return Ok(SplMint::unpack(&account.data)?.decimals);
    }
    if token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
        return Ok(StateWithExtensions::<Token2022Mint>::unpack(&account.data)?
            .base
            .decimals);
    }
    Err("unsupported canonical token program".into())
}

#[allow(clippy::too_many_arguments)]
fn transfer_checked_instruction(
    token_program: Pubkey,
    source: Pubkey,
    mint: Pubkey,
    destination: Pubkey,
    authority: Pubkey,
    amount: u64,
    decimals: u8,
) -> Result<Instruction, Box<dyn Error>> {
    if token_program == spl_token::id() {
        return Ok(spl_token::instruction::transfer_checked(
            &token_program,
            &source,
            &mint,
            &destination,
            &authority,
            &[],
            amount,
            decimals,
        )?);
    }
    if token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
        return Ok(spl_token_2022::instruction::transfer_checked(
            &token_program,
            &source,
            &mint,
            &destination,
            &authority,
            &[],
            amount,
            decimals,
        )?);
    }
    Err("unsupported token program for transfer".into())
}

fn close_token_account_instruction(
    token_program: Pubkey,
    account: Pubkey,
    destination: Pubkey,
    authority: Pubkey,
) -> Result<Instruction, Box<dyn Error>> {
    if token_program == spl_token::id() {
        return Ok(spl_token::instruction::close_account(
            &token_program,
            &account,
            &destination,
            &authority,
            &[],
        )?);
    }
    if token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
        return Ok(spl_token_2022::instruction::close_account(
            &token_program,
            &account,
            &destination,
            &authority,
            &[],
        )?);
    }
    Err("unsupported token program for close".into())
}

fn pair_key(pair: EarnStablecoinPair) -> Result<String, Box<dyn Error>> {
    let input = earn_stablecoins()
        .iter()
        .find(|asset| asset.mint == pair.input_mint)
        .ok_or("input pair mint is not canonical")?;
    let output = earn_stablecoins()
        .iter()
        .find(|asset| asset.mint == pair.output_mint)
        .ok_or("output pair mint is not canonical")?;
    Ok(format!("{}->{}", input.symbol, output.symbol))
}

const fn dialect_name(dialect: JupiterV2Dialect) -> &'static str {
    match dialect {
        JupiterV2Dialect::RouteV2 => "route_v2",
        JupiterV2Dialect::SharedAccountsRouteV2 => "shared_accounts_route_v2",
    }
}

fn anchor_account_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("account:{name}");
    solana_sdk::hash::hashv(&[preimage.as_bytes()]).to_bytes()[..8]
        .try_into()
        .expect("Anchor discriminator is eight bytes")
}

mod historical_routes {
    use super::*;
    use klend_interface::{
        from_account_data,
        instructions::{
            deposit::{
                deposit_reserve_liquidity_and_obligation_collateral_v2,
                DepositReserveLiquidityAndObligationCollateralV2Accounts,
            },
            obligation::{init_obligation, InitObligationAccounts},
            referrer::{init_user_metadata, InitUserMetadataAccounts},
            refresh::{
                refresh_obligation, refresh_reserve, RefreshObligationAccounts,
                RefreshReserveAccounts,
            },
            withdraw::{
                withdraw_obligation_collateral_and_redeem_reserve_collateral_v2,
                WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts,
            },
        },
        pda::{farms_user_state, lending_market_authority, obligation, user_metadata},
        state::{Obligation, Reserve},
        types::InitObligationArgs,
        FARMS_PROGRAM_ID, KLEND_PROGRAM_ID,
    };
    use loyal_actions::{
        create_init_obligation_yield_route_action, create_same_mint_market_mint_yield_route_action,
        kamino_init_obligation_farm_instruction, KaminoInitObligationFarm, YieldRouteUniverse,
        KAMINO_ETHENA_MARKET, KAMINO_FIGURE_MARKET, KAMINO_MAIN_MARKET, KAMINO_MAPLE_MARKET,
        KAMINO_ONRE_MARKET,
    };
    use num_bigint::BigUint;
    use num_traits::{ToPrimitive, Zero};
    use solana_system_interface::instruction as system_instruction;

    const ROUTE_LIVE_GATE: &str = "CROSS_MINT_MAINNET_ROUTES_EXECUTE";
    const ROUTE_LIMIT_ENV: &str = "CROSS_MINT_MAINNET_ROUTE_LIMIT";
    const ROUTE_STATE_FILE_ENV: &str = "CROSS_MINT_MAINNET_ROUTE_STATE_FILE";
    const DEFAULT_ROUTE_STATE_FILE: &str =
        ".agents/cross-mint-mainnet-generalized-historical-routes.json";
    const BASE_CLASSIC_POLICY_SEED: u64 = 1;
    const BASE_TOKEN_2022_POLICY_SEED: u64 = 2;
    const SETUP_CLASSIC_POLICY_SEED: u64 = 3;
    const SETUP_TOKEN_2022_POLICY_SEED: u64 = 4;
    const SWAP_CLASSIC_POLICY_SEED: u64 = 5;
    const SWAP_TOKEN_2022_POLICY_SEED: u64 = 6;
    const ROUTE_AMOUNT_RAW: u64 = 10_000;
    const VAULT_SOL_FUNDING_LAMPORTS: u64 = 100_000_000;
    const SHARED_ALT_A: &str = "7i8VciRdgphzakobo5E6nsNsHZtqDVXRDE6k1iQqvLq";
    const SHARED_ALT_B: &str = "AKgVyHByNHG4nZUjyHQaCszQd5oXDudVUcevsKLx3ehT";

    #[derive(Clone, Copy)]
    struct HistoricalRoute {
        input_symbol: &'static str,
        output_symbol: &'static str,
        history_evidence: &'static str,
        source_reserve: &'static str,
        target_reserve: &'static str,
    }

    #[derive(Clone, Debug)]
    struct ReserveSummary {
        reserve: Pubkey,
        market: Pubkey,
        liquidity_mint: Pubkey,
        liquidity_token_program: Pubkey,
        liquidity_supply: Pubkey,
        collateral_mint: Pubkey,
        collateral_supply: Pubkey,
        collateral_farm: Option<Pubkey>,
        pyth_oracle: Option<Pubkey>,
        switchboard_price_oracle: Option<Pubkey>,
        switchboard_twap_oracle: Option<Pubkey>,
        scope_prices: Option<Pubkey>,
    }

    const HISTORICAL_ROUTES: [HistoricalRoute; 10] = [
        HistoricalRoute {
            input_symbol: "USDC",
            output_symbol: "USDS",
            history_evidence: "safe_substitution",
            source_reserve: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
            target_reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
        },
        HistoricalRoute {
            input_symbol: "USDS",
            output_symbol: "PYUSD",
            history_evidence: "direction_only_inference",
            source_reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
            target_reserve: "2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN",
        },
        HistoricalRoute {
            input_symbol: "PYUSD",
            output_symbol: "USDG",
            history_evidence: "exact_historical_endpoints",
            source_reserve: "2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN",
            target_reserve: "JBmLCoKqjdKSStK45onRqe6U6sxVgSpdXoeXe4h7NwJw",
        },
        HistoricalRoute {
            input_symbol: "USDG",
            output_symbol: "USDS",
            history_evidence: "exact_historical_endpoints",
            source_reserve: "JBmLCoKqjdKSStK45onRqe6U6sxVgSpdXoeXe4h7NwJw",
            target_reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
        },
        HistoricalRoute {
            input_symbol: "USDS",
            output_symbol: "USDG",
            history_evidence: "safe_substitution_current_capacity",
            source_reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
            target_reserve: "JBmLCoKqjdKSStK45onRqe6U6sxVgSpdXoeXe4h7NwJw",
        },
        HistoricalRoute {
            input_symbol: "USDG",
            output_symbol: "USDC",
            history_evidence: "exact_historical_endpoints",
            source_reserve: "JBmLCoKqjdKSStK45onRqe6U6sxVgSpdXoeXe4h7NwJw",
            target_reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
        },
        HistoricalRoute {
            input_symbol: "USDC",
            output_symbol: "PYUSD",
            history_evidence: "safe_substitution",
            source_reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
            target_reserve: "2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN",
        },
        HistoricalRoute {
            input_symbol: "PYUSD",
            output_symbol: "USDC",
            history_evidence: "safe_substitution",
            source_reserve: "2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN",
            target_reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
        },
        HistoricalRoute {
            input_symbol: "USDC",
            output_symbol: "USDG",
            history_evidence: "exact_historical_endpoints",
            source_reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
            target_reserve: "JBmLCoKqjdKSStK45onRqe6U6sxVgSpdXoeXe4h7NwJw",
        },
        HistoricalRoute {
            input_symbol: "USDS",
            output_symbol: "USDC",
            history_evidence: "safe_substitution",
            source_reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
            target_reserve: "D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59",
        },
    ];

    #[test]
    #[ignore = "mutates mainnet; requires the explicit historical-route wrapper"]
    fn historical_withdraw_swap_deposit_routes_finalize_on_mainnet() {
        if env::var(ROUTE_LIVE_GATE).ok().as_deref() != Some("1") {
            panic!("{ROUTE_LIVE_GATE}=1 is required; run the explicit historical-route wrapper");
        }
        if env::var(CONFIRM_MAINNET_ENV).ok().as_deref() != Some("1") {
            panic!("mutating mainnet verifier requires {CONFIRM_MAINNET_ENV}=1");
        }
        if let Err(error) = run_historical_routes() {
            panic!("{}", redacted_external_error(&error.to_string()));
        }
    }

    fn run_historical_routes() -> Result<(), Box<dyn Error>> {
        let wallet = solana_testing_keypair_from_env()?;
        let rpc_url = env::var("SOLANA_RPC_URL")?;
        let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::finalized());
        validate_rpc_genesis_hash("mainnet-beta", rpc.get_genesis_hash()?)?;
        if rpc.get_balance(&wallet.pubkey())? < 100_000_000 {
            return Err("test wallet needs at least 0.1 SOL for historical routes".into());
        }
        let state_path = env::var(ROUTE_STATE_FILE_ENV).map_or_else(
            |_| {
                PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                    .join("../..")
                    .join(DEFAULT_ROUTE_STATE_FILE)
            },
            PathBuf::from,
        );
        let mut state = load_state(&state_path, wallet.pubkey())?;
        let smart_account = ensure_smart_account(&rpc, &wallet, &state_path, &mut state)?;
        let settings = Pubkey::from_str(&smart_account.settings)?;
        let vault = Pubkey::from_str(&smart_account.vault)?;
        if completed_route_count(&state) == HISTORICAL_ROUTES.len() {
            cleanup_historical_route_account(
                &rpc,
                &wallet,
                settings,
                vault,
                &state_path,
                &mut state,
            )?;
            verify_historical_cleanup(&rpc, vault, &state)?;
            eprintln!(
                "mainnet_historical_cross_mint_routes progress={}/{} settings={} vault={} cleanup=true verdict=PASS",
                completed_route_count(&state),
                HISTORICAL_ROUTES.len(),
                settings,
                vault,
            );
            return Ok(());
        }
        ensure_vault_sol_funding(&rpc, &wallet, vault, &state_path, &mut state)?;
        ensure_vault_funding(&rpc, &wallet, vault, &state_path, &mut state)?;
        ensure_route_policies(&rpc, &wallet, settings, vault, &state_path, &mut state)?;
        ensure_user_metadata(&rpc, &wallet, settings, vault, &state_path, &mut state)?;

        let lookup_tables = shared_lookup_tables(&rpc)?;
        let http = reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(30))
            .build()?;
        let route_limit = env::var(ROUTE_LIMIT_ENV)
            .ok()
            .map(|value| value.parse::<usize>())
            .transpose()?
            .unwrap_or(HISTORICAL_ROUTES.len());
        if !(1..=HISTORICAL_ROUTES.len()).contains(&route_limit) {
            return Err(format!(
                "{ROUTE_LIMIT_ENV} must be in 1..={}",
                HISTORICAL_ROUTES.len()
            )
            .into());
        }

        for route in HISTORICAL_ROUTES.iter().copied().take(route_limit) {
            execute_historical_route(
                &rpc,
                &http,
                &wallet,
                vault,
                &lookup_tables,
                route,
                &state_path,
                &mut state,
            )?;
        }

        if route_limit == HISTORICAL_ROUTES.len()
            && completed_route_count(&state) == HISTORICAL_ROUTES.len()
        {
            cleanup_historical_route_account(
                &rpc,
                &wallet,
                settings,
                vault,
                &state_path,
                &mut state,
            )?;
        }
        eprintln!(
            "mainnet_historical_cross_mint_routes progress={}/{} settings={} vault={} cleanup={} verdict=PASS",
            completed_route_count(&state),
            HISTORICAL_ROUTES.len(),
            settings,
            vault,
            route_limit == HISTORICAL_ROUTES.len()
                && completed_route_count(&state) == HISTORICAL_ROUTES.len(),
        );
        Ok(())
    }

    fn verify_historical_cleanup(
        rpc: &RpcClient,
        vault: Pubkey,
        state: &MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        if rpc.get_balance(&vault)? != 0 {
            return Err("test vault retains SOL after recorded cleanup".into());
        }
        for asset in earn_stablecoins().iter().copied() {
            let ata = derive_associated_token_account(vault, asset.mint, asset.token_program);
            if rpc
                .get_account_with_commitment(&ata, CommitmentConfig::finalized())?
                .value
                .is_some()
            {
                return Err(format!("{} ATA exists after recorded cleanup", asset.symbol).into());
            }
        }
        for policy in state
            .route_policies
            .values()
            .map(|evidence| evidence.policy_account.as_str())
            .chain(
                state
                    .cross_mint_policies
                    .values()
                    .map(|evidence| evidence.policy_account.as_str()),
            )
        {
            let policy = Pubkey::from_str(policy)?;
            if rpc
                .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
                .value
                .is_some()
            {
                return Err(format!("policy {policy} exists after recorded cleanup").into());
            }
        }
        if state.route_auxiliary_cleanup.len() != state.route_auxiliary_accounts.len() {
            return Err("auxiliary Kamino account cleanup disposition is incomplete".into());
        }
        Ok(())
    }

    fn ensure_vault_sol_funding(
        rpc: &RpcClient,
        wallet: &Keypair,
        vault: Pubkey,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        let stage = "route-setup:fund-vault-sol";
        let current = rpc.get_balance(&vault)?;
        let pending_stage = state.pending.as_ref().map(|pending| pending.stage.as_str());
        if pending_stage.is_some() && pending_stage != Some(stage) {
            // A value leg may already have finalized while its local evidence
            // write was interrupted. Let the owning route handler reconcile
            // that exact persisted wire before considering a setup top-up.
            return Ok(());
        }
        if current >= VAULT_SOL_FUNDING_LAMPORTS && pending_stage != Some(stage) {
            return Ok(());
        }
        let amount = VAULT_SOL_FUNDING_LAMPORTS.saturating_sub(current);
        let instruction = system_instruction::transfer(&wallet.pubkey(), &vault, amount);
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[instruction])?;
        let (evidence, _) = send_route_stage(
            rpc,
            state_path,
            state,
            stage,
            transaction,
            blockhash,
            last_valid_block_height,
            None,
        )?;
        if rpc.get_balance(&vault)? < VAULT_SOL_FUNDING_LAMPORTS {
            return Err("vault SOL funding did not finalize".into());
        }
        state.route_setup.insert(stage.to_owned(), evidence);
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    fn ensure_route_policies(
        rpc: &RpcClient,
        wallet: &Keypair,
        settings: Pubkey,
        vault: Pubkey,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        let context = LoyalActionContext {
            settings,
            authority: wallet.pubkey(),
            delegated_signer: wallet.pubkey(),
            account_index: VAULT_INDEX,
            vault,
        };
        let classic_universe = policy_universe(spl_token::id())?;
        let token_2022_universe = policy_universe(loyal_actions::TOKEN_2022_PROGRAM_ID)?;

        let base_classic = create_same_mint_market_mint_yield_route_action(
            context,
            classic_universe.clone(),
            BASE_CLASSIC_POLICY_SEED,
        )?;
        let [base_classic_instruction] = base_classic.instructions.as_slice() else {
            return Err("classic base policy SDK emitted an unexpected instruction count".into());
        };
        ensure_route_policy(
            rpc,
            wallet,
            settings,
            "base-classic",
            BASE_CLASSIC_POLICY_SEED,
            base_classic.accounts.withdraw,
            base_classic_instruction.clone(),
            state_path,
            state,
        )?;

        let base_token_2022 = create_same_mint_market_mint_yield_route_action(
            context,
            token_2022_universe.clone(),
            BASE_TOKEN_2022_POLICY_SEED,
        )?;
        let [base_token_2022_instruction] = base_token_2022.instructions.as_slice() else {
            return Err(
                "Token-2022 base policy SDK emitted an unexpected instruction count".into(),
            );
        };
        ensure_route_policy(
            rpc,
            wallet,
            settings,
            "base-token-2022",
            BASE_TOKEN_2022_POLICY_SEED,
            base_token_2022.accounts.withdraw,
            base_token_2022_instruction.clone(),
            state_path,
            state,
        )?;

        let setup_classic = create_init_obligation_yield_route_action(
            context,
            classic_universe,
            SETUP_CLASSIC_POLICY_SEED,
        )?;
        let [setup_classic_instruction] = setup_classic.instructions.as_slice() else {
            return Err("classic setup policy SDK emitted an unexpected instruction count".into());
        };
        ensure_route_policy(
            rpc,
            wallet,
            settings,
            "setup-classic",
            SETUP_CLASSIC_POLICY_SEED,
            setup_classic.accounts.deposit,
            setup_classic_instruction.clone(),
            state_path,
            state,
        )?;

        let setup_token_2022 = create_init_obligation_yield_route_action(
            context,
            token_2022_universe,
            SETUP_TOKEN_2022_POLICY_SEED,
        )?;
        let [setup_token_2022_instruction] = setup_token_2022.instructions.as_slice() else {
            return Err(
                "Token-2022 setup policy SDK emitted an unexpected instruction count".into(),
            );
        };
        ensure_route_policy(
            rpc,
            wallet,
            settings,
            "setup-token-2022",
            SETUP_TOKEN_2022_POLICY_SEED,
            setup_token_2022.accounts.deposit,
            setup_token_2022_instruction.clone(),
            state_path,
            state,
        )?;
        ensure_cross_mint_policy_set(
            rpc,
            wallet,
            settings,
            vault,
            JupiterCrossMintPolicySeeds {
                classic: SWAP_CLASSIC_POLICY_SEED,
                token_2022: SWAP_TOKEN_2022_POLICY_SEED,
            },
            state_path,
            state,
        )?;
        Ok(())
    }

    fn policy_universe(token_program: Pubkey) -> Result<YieldRouteUniverse, Box<dyn Error>> {
        let stable_mints = earn_stablecoins()
            .iter()
            .filter(|asset| asset.token_program == token_program)
            .map(|asset| asset.mint)
            .collect::<Vec<_>>();
        if stable_mints.is_empty() {
            return Err("policy shard token program has no canonical mints".into());
        }
        Ok(YieldRouteUniverse::new(
            stable_mints.clone(),
            vec![
                KAMINO_MAIN_MARKET,
                KAMINO_FIGURE_MARKET,
                KAMINO_MAPLE_MARKET,
                KAMINO_ONRE_MARKET,
                KAMINO_ETHENA_MARKET,
            ],
            stable_mints,
        ))
    }

    #[allow(clippy::too_many_arguments)]
    fn ensure_route_policy(
        rpc: &RpcClient,
        wallet: &Keypair,
        settings: Pubkey,
        label: &str,
        policy_seed: u64,
        policy_account: Pubkey,
        instruction: Instruction,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        if let Some(evidence) = state.route_policies.get(label) {
            if evidence.policy_seed != policy_seed
                || Pubkey::from_str(&evidence.policy_account)? != policy_account
            {
                return Err(format!("recorded {label} policy identity changed").into());
            }
            verify_program_interaction_policy_account(rpc, policy_account, settings)?;
            return Ok(());
        }
        let stage = format!("route-policy:create:{label}");
        if state.pending.as_ref().map(|pending| pending.stage.as_str()) != Some(stage.as_str()) {
            let settings_account = finalized_account(rpc, settings)?;
            let settings_wire = SettingsWire::try_from_slice(&settings_account.data)?;
            if settings_wire.policy_seed.unwrap_or(0).checked_add(1) != Some(policy_seed) {
                return Err(format!(
                    "{label} policy seed {policy_seed} is not the next finalized Settings seed"
                )
                .into());
            }
        }
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[instruction])?;
        let (transaction_evidence, _) = send_route_stage(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
            latest_finalized_policy_dependency_slot(state),
        )?;
        verify_program_interaction_policy_account(rpc, policy_account, settings)?;
        state.route_policies.insert(
            label.to_owned(),
            RoutePolicyEvidence {
                policy_account: policy_account.to_string(),
                policy_seed,
                transaction: transaction_evidence,
            },
        );
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    fn verify_program_interaction_policy_account(
        rpc: &RpcClient,
        policy: Pubkey,
        settings: Pubkey,
    ) -> Result<(), Box<dyn Error>> {
        let account = finalized_account(rpc, policy)?;
        if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID
            || !account
                .data
                .starts_with(&anchor_account_discriminator("Policy"))
            || account.data.get(8..40) != Some(settings.as_ref())
        {
            return Err(
                format!("policy {policy} failed finalized Squads owner/type readback").into(),
            );
        }
        Ok(())
    }

    fn ensure_user_metadata(
        rpc: &RpcClient,
        wallet: &Keypair,
        settings: Pubkey,
        vault: Pubkey,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        let (metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault);
        state
            .route_auxiliary_accounts
            .insert("kamino-user-metadata".to_owned(), metadata.to_string());
        save_state(state_path, state)?;
        let stage = "route-setup:init-user-metadata";
        if state.route_setup.contains_key(stage) {
            return verify_account_owner(rpc, metadata, KLEND_PROGRAM_ID, "user metadata");
        }
        if rpc
            .get_account_with_commitment(&metadata, CommitmentConfig::finalized())?
            .value
            .is_some()
            && state.pending.is_none()
        {
            return verify_account_owner(rpc, metadata, KLEND_PROGRAM_ID, "user metadata");
        }
        let inner = init_user_metadata(
            InitUserMetadataAccounts {
                owner: vault,
                fee_payer: vault,
                user_metadata: metadata,
                referrer_user_metadata: None,
            },
            Pubkey::default(),
        );
        let execute = wrap_settings_instruction(settings, wallet.pubkey(), inner);
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[execute])?;
        let (evidence, _) = send_route_stage(
            rpc,
            state_path,
            state,
            stage,
            transaction,
            blockhash,
            last_valid_block_height,
            None,
        )?;
        verify_account_owner(rpc, metadata, KLEND_PROGRAM_ID, "user metadata")?;
        state.route_setup.insert(stage.to_owned(), evidence);
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    fn wrap_settings_instruction(
        settings: Pubkey,
        authority: Pubkey,
        inner: Instruction,
    ) -> Instruction {
        let mut accounts = Vec::new();
        let compiled = compile_squads_inner_instruction(&mut accounts, inner);
        execute_sync_transaction_instruction(
            settings,
            authority,
            VAULT_INDEX,
            vec![compiled],
            accounts,
        )
    }

    #[allow(clippy::too_many_arguments)]
    fn send_route_stage(
        rpc: &RpcClient,
        state_path: &Path,
        state: &mut MainnetState,
        stage: &str,
        transaction: VersionedTransaction,
        blockhash: Hash,
        last_valid_block_height: u64,
        min_context_slot: Option<u64>,
    ) -> Result<(FinalizedTransactionEvidence, u64), Box<dyn Error>> {
        let units = if state.pending.as_ref().map(|pending| pending.stage.as_str()) == Some(stage) {
            0
        } else {
            simulate_signed_transaction(rpc, &transaction, stage, min_context_slot)?
        };
        let evidence = send_and_load_finalized(
            rpc,
            state_path,
            state,
            stage,
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        Ok((evidence, units))
    }

    #[derive(Clone, Copy)]
    enum LegKind {
        SourceDeposit,
        SourceWithdraw,
        TargetDeposit,
        TargetCleanupWithdraw,
    }

    #[allow(clippy::too_many_arguments)]
    fn execute_historical_route(
        rpc: &RpcClient,
        http: &reqwest::blocking::Client,
        wallet: &Keypair,
        vault: Pubkey,
        lookup_tables: &[AddressLookupTableAccount],
        route: HistoricalRoute,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        let route_key = format!("{}->{}", route.input_symbol, route.output_symbol);
        let input = asset_by_symbol(route.input_symbol)?;
        let output = asset_by_symbol(route.output_symbol)?;
        let pair = EarnStablecoinPair::new(input.mint, output.mint)
            .ok_or("historical route cannot be same-mint")?;
        let source = load_reserve_summary(rpc, Pubkey::from_str(route.source_reserve)?)?;
        let target = load_reserve_summary(rpc, Pubkey::from_str(route.target_reserve)?)?;
        require_route_reserve(&source, input, "source")?;
        require_route_reserve(&target, output, "target")?;
        require_safe_market(source.market)?;
        require_safe_market(target.market)?;

        let identity_changed = state
            .historical_routes
            .get(&route_key)
            .is_some_and(|existing| {
                existing.input_symbol != route.input_symbol
                    || existing.output_symbol != route.output_symbol
                    || existing.history_evidence != route.history_evidence
                    || existing.source_reserve != route.source_reserve
                    || existing.target_reserve != route.target_reserve
            });
        if identity_changed {
            let existing = state
                .historical_routes
                .get(&route_key)
                .ok_or("changed historical route disappeared")?;
            if state.pending.is_some() || route_has_value_evidence(existing) {
                return Err(format!(
                    "persisted route identity changed after value or signed-wire evidence for {route_key}"
                )
                .into());
            }
            state.historical_routes.remove(&route_key);
        }
        if !state.historical_routes.contains_key(&route_key) {
            state.historical_routes.insert(
                route_key.clone(),
                HistoricalRouteProgress {
                    input_symbol: route.input_symbol.to_owned(),
                    output_symbol: route.output_symbol.to_owned(),
                    history_evidence: route.history_evidence.to_owned(),
                    source_reserve: route.source_reserve.to_owned(),
                    target_reserve: route.target_reserve.to_owned(),
                    source_deposit: RouteLegProgress::default(),
                    source_withdraw: RouteLegProgress::default(),
                    swap_plan: None,
                    swap: None,
                    target_deposit: RouteLegProgress::default(),
                    target_cleanup_withdraw: RouteLegProgress::default(),
                },
            );
            save_state(state_path, state)?;
        }

        if leg_progress(state, &route_key, LegKind::SourceDeposit)?
            .evidence
            .is_none()
        {
            ensure_obligation_and_farm(
                rpc,
                wallet,
                vault,
                &source,
                lookup_tables,
                &format!("{route_key}:source"),
                state_path,
                state,
            )?;
        }
        execute_deposit_leg(
            rpc,
            wallet,
            vault,
            &source,
            ROUTE_AMOUNT_RAW,
            lookup_tables,
            &route_key,
            LegKind::SourceDeposit,
            state_path,
            state,
        )?;
        execute_withdraw_leg(
            rpc,
            wallet,
            vault,
            &source,
            lookup_tables,
            &route_key,
            LegKind::SourceWithdraw,
            state_path,
            state,
        )?;
        let withdrawn_amount = leg_progress(state, &route_key, LegKind::SourceWithdraw)?
            .evidence
            .as_ref()
            .ok_or("source withdraw evidence missing")?
            .finalized_token_delta_raw;
        let withdrawn_amount = u64::try_from(withdrawn_amount)
            .map_err(|_| "source withdraw did not finalize a positive token credit")?;
        execute_swap_leg(
            rpc,
            http,
            wallet,
            vault,
            pair,
            withdrawn_amount,
            &route_key,
            state_path,
            state,
        )?;
        let target_amount = state
            .historical_routes
            .get(&route_key)
            .and_then(|progress| progress.swap.as_ref())
            .map(|swap| swap.finalized_target_credit_raw)
            .ok_or("swap evidence missing before target deposit")?;

        if leg_progress(state, &route_key, LegKind::TargetDeposit)?
            .evidence
            .is_none()
        {
            ensure_obligation_and_farm(
                rpc,
                wallet,
                vault,
                &target,
                lookup_tables,
                &format!("{route_key}:target"),
                state_path,
                state,
            )?;
        }
        execute_deposit_leg(
            rpc,
            wallet,
            vault,
            &target,
            target_amount,
            lookup_tables,
            &route_key,
            LegKind::TargetDeposit,
            state_path,
            state,
        )?;
        execute_withdraw_leg(
            rpc,
            wallet,
            vault,
            &target,
            lookup_tables,
            &route_key,
            LegKind::TargetCleanupWithdraw,
            state_path,
            state,
        )?;

        eprintln!(
            "mainnet_historical_route={route_key} history_evidence={} source_reserve={} target_reserve={} withdraw={} swap={} deposit={} cleanup_withdraw={} verdict=PASS",
            route.history_evidence,
            source.reserve,
            target.reserve,
            leg_progress(state, &route_key, LegKind::SourceWithdraw)?
                .evidence
                .as_ref()
                .map(|evidence| evidence.transaction.signature.as_str())
                .unwrap_or("missing"),
            state
                .historical_routes
                .get(&route_key)
                .and_then(|progress| progress.swap.as_ref())
                .map(|swap| swap.swap.signature.as_str())
                .unwrap_or("missing"),
            leg_progress(state, &route_key, LegKind::TargetDeposit)?
                .evidence
                .as_ref()
                .map(|evidence| evidence.transaction.signature.as_str())
                .unwrap_or("missing"),
            leg_progress(state, &route_key, LegKind::TargetCleanupWithdraw)?
                .evidence
                .as_ref()
                .map(|evidence| evidence.transaction.signature.as_str())
                .unwrap_or("missing"),
        );
        Ok(())
    }

    fn route_has_value_evidence(route: &HistoricalRouteProgress) -> bool {
        route.source_deposit.evidence.is_some()
            || route.source_withdraw.evidence.is_some()
            || route.swap.is_some()
            || route.target_deposit.evidence.is_some()
            || route.target_cleanup_withdraw.evidence.is_some()
    }

    #[allow(clippy::too_many_arguments)]
    fn execute_deposit_leg(
        rpc: &RpcClient,
        wallet: &Keypair,
        vault: Pubkey,
        reserve: &ReserveSummary,
        amount: u64,
        lookup_tables: &[AddressLookupTableAccount],
        route_key: &str,
        kind: LegKind,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        if leg_progress(state, route_key, kind)?.evidence.is_some() {
            return Ok(());
        }
        ensure_leg_anchor(rpc, vault, reserve, route_key, kind, state_path, state)?;
        let before = leg_progress(state, route_key, kind)?
            .before
            .clone()
            .ok_or("deposit leg anchor missing")?;
        if !before.obligation_exists || before.deposited_collateral_amount_raw != 0 {
            return Err(
                format!("{route_key} deposit requires an empty existing obligation").into(),
            );
        }
        if before.token_amount_raw < amount {
            return Err(format!("{route_key} deposit token balance is below {amount}").into());
        }
        let policy =
            route_policy_account_for_token_program(state, "base", reserve.liquidity_token_program)?;
        let deposit = build_deposit_instruction(vault, reserve, amount);
        let wrapped = wrap_policy_instruction(policy, wallet.pubkey(), 1, deposit);
        let instructions = vec![
            ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
            build_refresh_reserve_instruction(reserve),
            build_refresh_obligation_instruction(vault, reserve, &[]),
            wrapped,
        ];
        let (transaction, blockhash, last_valid_block_height, _) =
            v0_transaction(rpc, wallet, &instructions, lookup_tables)?;
        let stage = format!("route:{route_key}:{}", leg_label(kind));
        let (transaction_evidence, simulated_units_consumed) = send_route_stage(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
            Some(before.finalized_context_slot),
        )?;
        let after =
            route_leg_anchor_at_or_after(rpc, vault, reserve, transaction_evidence.finalized_slot)?;
        let actual_debit = before
            .token_amount_raw
            .checked_sub(after.token_amount_raw)
            .ok_or("deposit increased the anchored token balance")?;
        if actual_debit == 0
            || actual_debit > amount
            || after.deposited_collateral_amount_raw <= before.deposited_collateral_amount_raw
        {
            return Err(format!(
                "{route_key} deposit failed reconciliation: requested {amount}, token {}->{}, collateral {}->{}",
                before.token_amount_raw,
                after.token_amount_raw,
                before.deposited_collateral_amount_raw,
                after.deposited_collateral_amount_raw,
            )
            .into());
        }
        let residual = amount - actual_debit;
        // SourceDeposit is test scaffolding: Kamino may admit less than the
        // requested setup amount when a live reserve is near capacity, and
        // the following withdraw defines the movement-attributed swap input.
        // TargetDeposit is the actual terminal route leg, so any remainder
        // large enough for another deposit must still fail reconciliation.
        if residual > 0 && matches!(kind, LegKind::TargetDeposit) {
            let minimum_deposit = after
                .minimum_deposit_amount_raw
                .ok_or("partial deposit lacks finalized minimum-deposit evidence")?;
            if residual >= minimum_deposit {
                return Err(format!(
                    "{route_key} deposit left {residual} raw units, enough to meet the finalized {minimum_deposit}-unit Kamino minimum"
                )
                .into());
            }
        }
        let evidence = RouteLegEvidence {
            requested_amount_raw: amount,
            finalized_token_delta_raw: i128::from(after.token_amount_raw)
                - i128::from(before.token_amount_raw),
            before,
            after,
            simulated_units_consumed,
            transaction: transaction_evidence,
        };
        leg_progress_mut(state, route_key, kind)?.evidence = Some(evidence);
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    fn execute_withdraw_leg(
        rpc: &RpcClient,
        wallet: &Keypair,
        vault: Pubkey,
        reserve: &ReserveSummary,
        lookup_tables: &[AddressLookupTableAccount],
        route_key: &str,
        kind: LegKind,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        if leg_progress(state, route_key, kind)?.evidence.is_some() {
            return Ok(());
        }
        ensure_leg_anchor(rpc, vault, reserve, route_key, kind, state_path, state)?;
        let before = leg_progress(state, route_key, kind)?
            .before
            .clone()
            .ok_or("withdraw leg anchor missing")?;
        let amount = before.deposited_collateral_amount_raw;
        if !before.obligation_exists || amount == 0 {
            return Err(
                format!("{route_key} withdraw requires a nonzero obligation position").into(),
            );
        }
        let policy =
            route_policy_account_for_token_program(state, "base", reserve.liquidity_token_program)?;
        let withdraw = build_withdraw_instruction(vault, reserve, amount);
        let wrapped = wrap_policy_instruction(policy, wallet.pubkey(), 0, withdraw);
        let instructions = vec![
            ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
            build_refresh_reserve_instruction(reserve),
            build_refresh_obligation_instruction(vault, reserve, &[reserve.reserve]),
            wrapped,
        ];
        let (transaction, blockhash, last_valid_block_height, _) =
            v0_transaction(rpc, wallet, &instructions, lookup_tables)?;
        let stage = format!("route:{route_key}:{}", leg_label(kind));
        let (transaction_evidence, simulated_units_consumed) = send_route_stage(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
            Some(before.finalized_context_slot),
        )?;
        let after =
            route_leg_anchor_at_or_after(rpc, vault, reserve, transaction_evidence.finalized_slot)?;
        if after.deposited_collateral_amount_raw != 0
            || after.token_amount_raw <= before.token_amount_raw
        {
            return Err(format!(
                "{route_key} withdraw failed reconciliation: token {}->{}, collateral {}->{}",
                before.token_amount_raw,
                after.token_amount_raw,
                before.deposited_collateral_amount_raw,
                after.deposited_collateral_amount_raw,
            )
            .into());
        }
        let evidence = RouteLegEvidence {
            requested_amount_raw: amount,
            finalized_token_delta_raw: i128::from(after.token_amount_raw)
                - i128::from(before.token_amount_raw),
            before,
            after,
            simulated_units_consumed,
            transaction: transaction_evidence,
        };
        leg_progress_mut(state, route_key, kind)?.evidence = Some(evidence);
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    fn execute_swap_leg(
        rpc: &RpcClient,
        http: &reqwest::blocking::Client,
        wallet: &Keypair,
        vault: Pubkey,
        pair: EarnStablecoinPair,
        amount: u64,
        route_key: &str,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        if state
            .historical_routes
            .get(route_key)
            .and_then(|route| route.swap.as_ref())
            .is_some()
        {
            return Ok(());
        }
        let stage = format!("route:{route_key}:swap");
        let (plan, transaction, blockhash, last_valid_block_height, packet_bytes) =
            if state.pending.as_ref().map(|pending| pending.stage.as_str()) == Some(stage.as_str())
            {
                let plan = state
                    .historical_routes
                    .get(route_key)
                    .and_then(|route| route.swap_plan.clone())
                    .ok_or("pending swap has no persisted certification plan")?;
                if plan.input_amount_raw != amount {
                    return Err(format!("{route_key} persisted swap input changed").into());
                }
                let pending = state.pending.as_ref().ok_or("pending swap disappeared")?;
                let wire = BASE64_STANDARD.decode(&pending.signed_wire_base64)?;
                let transaction = bincode::deserialize(&wire)?;
                (
                    plan,
                    transaction,
                    Hash::from_str(&pending.recent_blockhash)?,
                    pending.last_valid_block_height,
                    wire.len(),
                )
            } else {
                let prepared = prepare_swap(rpc, http, vault, pair, amount)?;
                let (policy_account, constraint_index, policy) =
                    select_cross_mint_policy(state, prepared.pair, prepared.validated.dialect)?;
                let plan = RouteSwapPlan {
                    input_amount_raw: amount,
                    minimum_output_amount_raw: prepared.minimum_output_amount,
                    dialect: dialect_name(prepared.validated.dialect).to_owned(),
                    route_step_count: prepared.validated.route_step_count,
                    unique_account_count: prepared.validated.structure.unique_account_count,
                    policy_account: policy.policy_account.clone(),
                    constraint_index,
                    policy_signature: policy.transaction.signature.clone(),
                    policy_finalized_slot: policy.transaction.finalized_slot,
                };
                state
                    .historical_routes
                    .get_mut(route_key)
                    .ok_or("historical route disappeared")?
                    .swap_plan = Some(plan.clone());
                save_state(state_path, state)?;
                let (transaction, blockhash, last_valid_block_height, packet_bytes) =
                    build_wrapped_swap_transaction(
                        rpc,
                        wallet,
                        policy_account,
                        constraint_index,
                        &prepared,
                    )?;
                (
                    plan,
                    transaction,
                    blockhash,
                    last_valid_block_height,
                    packet_bytes,
                )
            };
        let (swap_evidence, simulated_units_consumed) = send_route_stage(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
            Some(plan.policy_finalized_slot),
        )?;
        let (source_debit, target_credit) = finalized_pair_deltas(
            rpc,
            &swap_evidence,
            vault,
            pair.input_mint,
            pair.output_mint,
        )?;
        if source_debit != plan.input_amount_raw
            || target_credit < plan.minimum_output_amount_raw
            || target_credit == 0
        {
            return Err(format!(
                "{route_key} swap failed ExactIn reconciliation: debit={source_debit} credit={target_credit} minOut={}",
                plan.minimum_output_amount_raw
            )
            .into());
        }
        let input = asset_by_mint(pair.input_mint)?;
        let output = asset_by_mint(pair.output_mint)?;
        state
            .historical_routes
            .get_mut(route_key)
            .ok_or("historical route disappeared")?
            .swap = Some(PairEvidence {
            input_symbol: input.symbol.to_owned(),
            output_symbol: output.symbol.to_owned(),
            input_mint: input.mint.to_string(),
            output_mint: output.mint.to_string(),
            input_amount_raw: plan.input_amount_raw,
            minimum_output_amount_raw: plan.minimum_output_amount_raw,
            finalized_source_debit_raw: source_debit,
            finalized_target_credit_raw: target_credit,
            dialect: plan.dialect,
            route_step_count: plan.route_step_count,
            unique_account_count: plan.unique_account_count,
            wrapped_packet_bytes: packet_bytes,
            simulated_units_consumed,
            policy_account: plan.policy_account,
            policy_signature: plan.policy_signature,
            swap: swap_evidence,
        });
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    fn ensure_leg_anchor(
        rpc: &RpcClient,
        vault: Pubkey,
        reserve: &ReserveSummary,
        route_key: &str,
        kind: LegKind,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        if leg_progress(state, route_key, kind)?.before.is_some() {
            return Ok(());
        }
        let anchor = route_leg_anchor_at_or_after(rpc, vault, reserve, 0)?;
        leg_progress_mut(state, route_key, kind)?.before = Some(anchor);
        save_state(state_path, state)?;
        Ok(())
    }

    fn leg_progress<'a>(
        state: &'a MainnetState,
        route_key: &str,
        kind: LegKind,
    ) -> Result<&'a RouteLegProgress, Box<dyn Error>> {
        let route = state
            .historical_routes
            .get(route_key)
            .ok_or("historical route is missing")?;
        Ok(match kind {
            LegKind::SourceDeposit => &route.source_deposit,
            LegKind::SourceWithdraw => &route.source_withdraw,
            LegKind::TargetDeposit => &route.target_deposit,
            LegKind::TargetCleanupWithdraw => &route.target_cleanup_withdraw,
        })
    }

    fn leg_progress_mut<'a>(
        state: &'a mut MainnetState,
        route_key: &str,
        kind: LegKind,
    ) -> Result<&'a mut RouteLegProgress, Box<dyn Error>> {
        let route = state
            .historical_routes
            .get_mut(route_key)
            .ok_or("historical route is missing")?;
        Ok(match kind {
            LegKind::SourceDeposit => &mut route.source_deposit,
            LegKind::SourceWithdraw => &mut route.source_withdraw,
            LegKind::TargetDeposit => &mut route.target_deposit,
            LegKind::TargetCleanupWithdraw => &mut route.target_cleanup_withdraw,
        })
    }

    const fn leg_label(kind: LegKind) -> &'static str {
        match kind {
            LegKind::SourceDeposit => "source-deposit",
            LegKind::SourceWithdraw => "source-withdraw",
            LegKind::TargetDeposit => "target-deposit",
            LegKind::TargetCleanupWithdraw => "target-cleanup-withdraw",
        }
    }

    fn route_policy_account(state: &MainnetState, label: &str) -> Result<Pubkey, Box<dyn Error>> {
        Ok(Pubkey::from_str(
            &state
                .route_policies
                .get(label)
                .ok_or("route policy evidence is missing")?
                .policy_account,
        )?)
    }

    fn route_policy_account_for_token_program(
        state: &MainnetState,
        family: &str,
        token_program: Pubkey,
    ) -> Result<Pubkey, Box<dyn Error>> {
        let class = if token_program == spl_token::id() {
            "classic"
        } else if token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
            "token-2022"
        } else {
            return Err(format!("unsupported route token program {token_program}").into());
        };
        route_policy_account(state, &format!("{family}-{class}"))
    }

    fn asset_by_symbol(symbol: &str) -> Result<EarnStablecoin, Box<dyn Error>> {
        earn_stablecoins()
            .iter()
            .copied()
            .find(|asset| asset.symbol == symbol)
            .ok_or_else(|| format!("{symbol} is not a canonical Earn stablecoin").into())
    }

    fn asset_by_mint(mint: Pubkey) -> Result<EarnStablecoin, Box<dyn Error>> {
        earn_stablecoins()
            .iter()
            .copied()
            .find(|asset| asset.mint == mint)
            .ok_or_else(|| format!("{mint} is not a canonical Earn stablecoin").into())
    }

    #[allow(clippy::too_many_arguments)]
    fn ensure_obligation_and_farm(
        rpc: &RpcClient,
        wallet: &Keypair,
        vault: Pubkey,
        reserve: &ReserveSummary,
        lookup_tables: &[AddressLookupTableAccount],
        scope: &str,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        let obligation_address = derive_obligation(vault, reserve.market);
        state.route_auxiliary_accounts.insert(
            format!("kamino-obligation:{obligation_address}"),
            obligation_address.to_string(),
        );
        save_state(state_path, state)?;
        let init_stage = format!("route-setup:{scope}:init-obligation");
        let obligation_response =
            rpc.get_account_with_commitment(&obligation_address, CommitmentConfig::finalized())?;
        let mut obligation_min_context_slot = obligation_response.context.slot;
        let obligation_exists = obligation_response.value.is_some();
        if !obligation_exists
            || state.pending.as_ref().map(|pending| pending.stage.as_str())
                == Some(init_stage.as_str())
        {
            let inner = build_init_obligation_instruction(vault, reserve.market);
            let policy = route_policy_account_for_token_program(
                state,
                "setup",
                reserve.liquidity_token_program,
            )?;
            let wrapped = wrap_policy_instruction(policy, wallet.pubkey(), 1, inner);
            let instructions = vec![
                ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
                wrapped,
            ];
            let (transaction, blockhash, last_valid_block_height, _) =
                v0_transaction(rpc, wallet, &instructions, lookup_tables)?;
            let (evidence, _) = send_route_stage(
                rpc,
                state_path,
                state,
                &init_stage,
                transaction,
                blockhash,
                last_valid_block_height,
                None,
            )?;
            obligation_min_context_slot = obligation_min_context_slot.max(evidence.finalized_slot);
            state.route_setup.insert(init_stage.clone(), evidence);
            state.pending = None;
            save_state(state_path, state)?;
        }
        verify_obligation_identity(rpc, obligation_address, vault, reserve.market)?;

        let Some(farm) = reserve.collateral_farm else {
            return Ok(());
        };
        let (farm_user_state, _) = farms_user_state(&farm, &obligation_address);
        state.route_auxiliary_accounts.insert(
            format!("kamino-farm:{farm_user_state}"),
            farm_user_state.to_string(),
        );
        save_state(state_path, state)?;
        let farm_stage = format!("route-setup:{scope}:init-farm");
        let farm_exists = rpc
            .get_account_with_commitment(&farm_user_state, CommitmentConfig::finalized())?
            .value
            .is_some();
        if !farm_exists
            || state.pending.as_ref().map(|pending| pending.stage.as_str())
                == Some(farm_stage.as_str())
        {
            let instruction = kamino_init_obligation_farm_instruction(KaminoInitObligationFarm {
                payer: wallet.pubkey(),
                owner: vault,
                lending_market: reserve.market,
                reserve: reserve.reserve,
                reserve_farm_state: farm,
            });
            let instructions = vec![
                ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
                instruction,
            ];
            let (transaction, blockhash, last_valid_block_height, _) =
                v0_transaction(rpc, wallet, &instructions, lookup_tables)?;
            let (evidence, _) = send_route_stage(
                rpc,
                state_path,
                state,
                &farm_stage,
                transaction,
                blockhash,
                last_valid_block_height,
                Some(obligation_min_context_slot),
            )?;
            state.route_setup.insert(farm_stage.clone(), evidence);
            state.pending = None;
            save_state(state_path, state)?;
        }
        verify_account_owner(rpc, farm_user_state, FARMS_PROGRAM_ID, "obligation farm")?;
        Ok(())
    }

    fn route_leg_anchor_at_or_after(
        rpc: &RpcClient,
        vault: Pubkey,
        reserve: &ReserveSummary,
        minimum_slot: u64,
    ) -> Result<RouteLegAnchor, Box<dyn Error>> {
        let token_account = derive_associated_token_account(
            vault,
            reserve.liquidity_mint,
            reserve.liquidity_token_program,
        );
        let obligation_address = derive_obligation(vault, reserve.market);
        for attempt in 0..10 {
            let token_response =
                rpc.get_account_with_commitment(&token_account, CommitmentConfig::finalized())?;
            let obligation_response = rpc
                .get_account_with_commitment(&obligation_address, CommitmentConfig::finalized())?;
            let reserve_response =
                rpc.get_account_with_commitment(&reserve.reserve, CommitmentConfig::finalized())?;
            let context_slot = token_response
                .context
                .slot
                .min(obligation_response.context.slot)
                .min(reserve_response.context.slot);
            if context_slot < minimum_slot {
                if attempt == 9 {
                    return Err(format!(
                        "finalized Kamino readback slot {context_slot} is below transaction slot {minimum_slot}"
                    )
                    .into());
                }
                sleep(Duration::from_millis(500));
                continue;
            }
            let token = token_response
                .value
                .ok_or_else(|| format!("vault token account {token_account} is missing"))?;
            if token.owner != reserve.liquidity_token_program {
                return Err("vault token account owner differs from reserve token program".into());
            }
            let token_amount_raw = token_amount(&token, reserve.liquidity_token_program)?;
            let reserve_account = reserve_response
                .value
                .ok_or_else(|| format!("Kamino reserve {} is missing", reserve.reserve))?;
            if reserve_account.owner != KLEND_PROGRAM_ID {
                return Err("Kamino reserve has the wrong owner".into());
            }
            let reserve_state = from_account_data::<Reserve>(&reserve_account.data)?;
            if reserve_state.lending_market != reserve.market
                || reserve_state.liquidity.mint_pubkey != reserve.liquidity_mint
            {
                return Err("Kamino reserve identity changed during route reconciliation".into());
            }
            let (obligation_exists, deposited_collateral_amount_raw) =
                match obligation_response.value {
                    None => (false, 0),
                    Some(account) => {
                        if account.owner != KLEND_PROGRAM_ID {
                            return Err("Kamino obligation has the wrong owner".into());
                        }
                        let obligation_state = from_account_data::<Obligation>(&account.data)?;
                        if obligation_state.owner != vault
                            || obligation_state.lending_market != reserve.market
                        {
                            return Err("Kamino obligation identity differs from route".into());
                        }
                        let amount = obligation_state
                            .deposits
                            .iter()
                            .find(|deposit| deposit.deposit_reserve == reserve.reserve)
                            .map(|deposit| deposit.deposited_amount)
                            .unwrap_or_default();
                        (true, amount)
                    }
                };
            return Ok(RouteLegAnchor {
                token_amount_raw,
                obligation: obligation_address.to_string(),
                obligation_exists,
                deposited_collateral_amount_raw,
                minimum_deposit_amount_raw: Some(minimum_kamino_deposit_amount_raw(reserve_state)?),
                finalized_context_slot: context_slot,
            });
        }
        Err("finalized Kamino readback retry loop exhausted".into())
    }

    fn verify_obligation_identity(
        rpc: &RpcClient,
        address: Pubkey,
        vault: Pubkey,
        market: Pubkey,
    ) -> Result<(), Box<dyn Error>> {
        let account = finalized_account(rpc, address)?;
        if account.owner != KLEND_PROGRAM_ID {
            return Err(format!("obligation {address} has the wrong owner").into());
        }
        let obligation = from_account_data::<Obligation>(&account.data)?;
        if obligation.owner != vault || obligation.lending_market != market {
            return Err(format!("obligation {address} has the wrong owner or market").into());
        }
        Ok(())
    }

    fn verify_account_owner(
        rpc: &RpcClient,
        address: Pubkey,
        owner: Pubkey,
        label: &str,
    ) -> Result<(), Box<dyn Error>> {
        let account = finalized_account(rpc, address)?;
        if account.owner != owner {
            return Err(format!(
                "{label} {address} has owner {}, expected {owner}",
                account.owner
            )
            .into());
        }
        Ok(())
    }

    fn minimum_kamino_deposit_amount_raw(reserve: &Reserve) -> Result<u64, Box<dyn Error>> {
        let scale = BigUint::from(1_u128 << 60);
        let mut total_liquidity_scaled =
            BigUint::from(reserve.liquidity.total_available_amount) * &scale;
        total_liquidity_scaled += BigUint::from(u128::from(reserve.liquidity.borrowed_amount_sf));
        for (amount, label) in [
            (
                u128::from(reserve.liquidity.accumulated_protocol_fees_sf),
                "protocol fees",
            ),
            (
                u128::from(reserve.liquidity.accumulated_referrer_fees_sf),
                "referrer fees",
            ),
            (
                u128::from(reserve.liquidity.pending_referrer_fees_sf),
                "pending referrer fees",
            ),
        ] {
            let amount = BigUint::from(amount);
            if total_liquidity_scaled < amount {
                return Err(format!("Kamino reserve underflow subtracting {label}").into());
            }
            total_liquidity_scaled -= amount;
        }
        if reserve.collateral.mint_total_supply == 0 || total_liquidity_scaled.is_zero() {
            return Ok(1);
        }
        let denominator = BigUint::from(reserve.collateral.mint_total_supply) * scale;
        let numerator = total_liquidity_scaled + &denominator - BigUint::from(1_u8);
        (numerator / denominator)
            .to_u64()
            .filter(|amount| *amount > 0)
            .ok_or_else(|| "Kamino minimum deposit does not fit positive u64".into())
    }

    fn load_reserve_summary(
        rpc: &RpcClient,
        reserve: Pubkey,
    ) -> Result<ReserveSummary, Box<dyn Error>> {
        let account = finalized_account(rpc, reserve)?;
        if account.owner != KLEND_PROGRAM_ID {
            return Err(format!("reserve {reserve} has the wrong owner").into());
        }
        let state = from_account_data::<Reserve>(&account.data)?;
        Ok(ReserveSummary {
            reserve,
            market: state.lending_market,
            liquidity_mint: state.liquidity.mint_pubkey,
            liquidity_token_program: state.liquidity.token_program,
            liquidity_supply: state.liquidity.supply_vault,
            collateral_mint: state.collateral.mint_pubkey,
            collateral_supply: state.collateral.supply_vault,
            collateral_farm: non_default_pubkey(state.farm_collateral),
            pyth_oracle: non_default_pubkey(state.config.token_info.pyth_configuration.price),
            switchboard_price_oracle: non_default_pubkey(
                state
                    .config
                    .token_info
                    .switchboard_configuration
                    .price_aggregator,
            ),
            switchboard_twap_oracle: non_default_pubkey(
                state
                    .config
                    .token_info
                    .switchboard_configuration
                    .twap_aggregator,
            ),
            scope_prices: non_default_pubkey(
                state.config.token_info.scope_configuration.price_feed,
            ),
        })
    }

    fn require_route_reserve(
        reserve: &ReserveSummary,
        asset: EarnStablecoin,
        label: &str,
    ) -> Result<(), Box<dyn Error>> {
        if reserve.liquidity_mint != asset.mint
            || reserve.liquidity_token_program != asset.token_program
        {
            return Err(format!(
                "{label} reserve {} is {} via {}, expected {} via {}",
                reserve.reserve,
                reserve.liquidity_mint,
                reserve.liquidity_token_program,
                asset.mint,
                asset.token_program,
            )
            .into());
        }
        Ok(())
    }

    fn require_safe_market(market: Pubkey) -> Result<(), Box<dyn Error>> {
        if ![
            KAMINO_MAIN_MARKET,
            KAMINO_FIGURE_MARKET,
            KAMINO_MAPLE_MARKET,
            KAMINO_ONRE_MARKET,
            KAMINO_ETHENA_MARKET,
        ]
        .contains(&market)
        {
            return Err(format!("market {market} is outside the Safe policy basket").into());
        }
        Ok(())
    }

    fn build_init_obligation_instruction(vault: Pubkey, market: Pubkey) -> Instruction {
        let (owner_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault);
        init_obligation(
            InitObligationAccounts {
                obligation_owner: vault,
                fee_payer: vault,
                obligation: derive_obligation(vault, market),
                lending_market: market,
                seed1_account: Pubkey::default(),
                seed2_account: Pubkey::default(),
                owner_user_metadata,
            },
            InitObligationArgs { tag: 0, id: 0 },
        )
    }

    fn build_deposit_instruction(
        vault: Pubkey,
        reserve: &ReserveSummary,
        amount: u64,
    ) -> Instruction {
        let obligation = derive_obligation(vault, reserve.market);
        let (market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &reserve.market);
        let (obligation_farm_user_state, reserve_farm_state) =
            collateral_farm_accounts(reserve.collateral_farm, obligation);
        deposit_reserve_liquidity_and_obligation_collateral_v2(
            DepositReserveLiquidityAndObligationCollateralV2Accounts {
                owner: vault,
                obligation,
                lending_market: reserve.market,
                lending_market_authority: market_authority,
                reserve: reserve.reserve,
                reserve_liquidity_mint: reserve.liquidity_mint,
                reserve_liquidity_supply: reserve.liquidity_supply,
                reserve_collateral_mint: reserve.collateral_mint,
                reserve_destination_deposit_collateral: reserve.collateral_supply,
                user_source_liquidity: derive_associated_token_account(
                    vault,
                    reserve.liquidity_mint,
                    reserve.liquidity_token_program,
                ),
                placeholder_user_destination_collateral: None,
                liquidity_token_program: reserve.liquidity_token_program,
                obligation_farm_user_state,
                reserve_farm_state,
            },
            amount,
        )
    }

    fn build_withdraw_instruction(
        vault: Pubkey,
        reserve: &ReserveSummary,
        collateral_amount: u64,
    ) -> Instruction {
        let obligation = derive_obligation(vault, reserve.market);
        let (market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &reserve.market);
        let (obligation_farm_user_state, reserve_farm_state) =
            collateral_farm_accounts(reserve.collateral_farm, obligation);
        withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
                owner: vault,
                obligation,
                lending_market: reserve.market,
                lending_market_authority: market_authority,
                withdraw_reserve: reserve.reserve,
                reserve_liquidity_mint: reserve.liquidity_mint,
                reserve_source_collateral: reserve.collateral_supply,
                reserve_collateral_mint: reserve.collateral_mint,
                reserve_liquidity_supply: reserve.liquidity_supply,
                user_destination_liquidity: derive_associated_token_account(
                    vault,
                    reserve.liquidity_mint,
                    reserve.liquidity_token_program,
                ),
                placeholder_user_destination_collateral: None,
                liquidity_token_program: reserve.liquidity_token_program,
                obligation_farm_user_state,
                reserve_farm_state,
            },
            collateral_amount,
        )
    }

    fn build_refresh_reserve_instruction(reserve: &ReserveSummary) -> Instruction {
        refresh_reserve(RefreshReserveAccounts {
            reserve: reserve.reserve,
            lending_market: reserve.market,
            pyth_oracle: reserve.pyth_oracle,
            switchboard_price_oracle: reserve.switchboard_price_oracle,
            switchboard_twap_oracle: reserve.switchboard_twap_oracle,
            scope_prices: reserve.scope_prices,
        })
    }

    fn build_refresh_obligation_instruction(
        vault: Pubkey,
        reserve: &ReserveSummary,
        remaining_reserves: &[Pubkey],
    ) -> Instruction {
        refresh_obligation(
            RefreshObligationAccounts {
                lending_market: reserve.market,
                obligation: derive_obligation(vault, reserve.market),
            },
            remaining_reserves
                .iter()
                .map(|reserve| AccountMeta::new(*reserve, false))
                .collect(),
        )
    }

    fn wrap_policy_instruction(
        policy: Pubkey,
        delegated_signer: Pubkey,
        constraint_index: u8,
        inner: Instruction,
    ) -> Instruction {
        let mut accounts = Vec::new();
        let compiled = compile_squads_inner_instruction(&mut accounts, inner);
        execute_program_interaction_policy_instruction(
            policy,
            delegated_signer,
            VAULT_INDEX,
            vec![compiled],
            vec![constraint_index],
            accounts,
        )
    }

    fn derive_obligation(vault: Pubkey, market: Pubkey) -> Pubkey {
        obligation(
            &KLEND_PROGRAM_ID,
            0,
            0,
            &vault,
            &market,
            &Pubkey::default(),
            &Pubkey::default(),
        )
        .0
    }

    fn collateral_farm_accounts(
        farm: Option<Pubkey>,
        obligation: Pubkey,
    ) -> (Option<Pubkey>, Option<Pubkey>) {
        let Some(farm) = farm else {
            return (None, None);
        };
        (Some(farms_user_state(&farm, &obligation).0), Some(farm))
    }

    fn non_default_pubkey(pubkey: Pubkey) -> Option<Pubkey> {
        (pubkey != Pubkey::default()).then_some(pubkey)
    }

    fn shared_lookup_tables(
        rpc: &RpcClient,
    ) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
        [SHARED_ALT_A, SHARED_ALT_B]
            .iter()
            .map(|address| finalized_lookup_table(rpc, Pubkey::from_str(address)?))
            .collect()
    }

    fn finalized_lookup_table(
        rpc: &RpcClient,
        address: Pubkey,
    ) -> Result<AddressLookupTableAccount, Box<dyn Error>> {
        let account = finalized_account(rpc, address)?;
        if account.owner != address_lookup_table_program::id() {
            return Err(format!("shared ALT {address} has the wrong owner").into());
        }
        let table = AddressLookupTable::deserialize(&account.data)?;
        if table.meta.deactivation_slot != u64::MAX {
            return Err(format!("shared ALT {address} is deactivating").into());
        }
        Ok(AddressLookupTableAccount {
            key: address,
            addresses: table.addresses.iter().copied().collect(),
        })
    }

    fn v0_transaction(
        rpc: &RpcClient,
        wallet: &Keypair,
        instructions: &[Instruction],
        lookup_tables: &[AddressLookupTableAccount],
    ) -> Result<(VersionedTransaction, Hash, u64, usize), Box<dyn Error>> {
        let (blockhash, last_valid_block_height) =
            rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
        let message =
            v0::Message::try_compile(&wallet.pubkey(), instructions, lookup_tables, blockhash)?;
        let transaction = VersionedTransaction::try_new(VersionedMessage::V0(message), &[wallet])?;
        let packet_bytes = bincode::serialize(&transaction)?.len();
        if packet_bytes > SOLANA_PACKET_DATA_SIZE {
            return Err(format!("route transaction is {packet_bytes} bytes").into());
        }
        Ok((
            transaction,
            blockhash,
            last_valid_block_height,
            packet_bytes,
        ))
    }

    fn cleanup_historical_route_account(
        rpc: &RpcClient,
        wallet: &Keypair,
        settings: Pubkey,
        vault: Pubkey,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        for route in state.historical_routes.values() {
            for leg in [&route.source_withdraw, &route.target_cleanup_withdraw] {
                let Some(evidence) = leg.evidence.as_ref() else {
                    return Err("cleanup requires every withdraw reconciliation".into());
                };
                if evidence.after.deposited_collateral_amount_raw != 0 {
                    return Err("cleanup found a nonzero Kamino position".into());
                }
            }
        }
        cleanup_vault(rpc, wallet, settings, vault, state_path, state)?;
        for label in [
            "base-classic",
            "base-token-2022",
            "setup-classic",
            "setup-token-2022",
        ] {
            remove_route_policy(rpc, wallet, settings, label, state_path, state)?;
        }
        let stage = "cleanup:recover-vault-sol";
        if !state.cleanup.contains_key(stage) {
            let balance = rpc.get_balance(&vault)?;
            if balance > 0 || state.pending.is_some() {
                let transfer = system_instruction::transfer(&vault, &wallet.pubkey(), balance);
                let execute = wrap_settings_instruction(settings, wallet.pubkey(), transfer);
                let (transaction, blockhash, last_valid_block_height) =
                    legacy_transaction(rpc, wallet, &[execute])?;
                let (evidence, _) = send_route_stage(
                    rpc,
                    state_path,
                    state,
                    stage,
                    transaction,
                    blockhash,
                    last_valid_block_height,
                    None,
                )?;
                state.cleanup.insert(stage.to_owned(), evidence);
                state.pending = None;
                save_state(state_path, state)?;
            }
        }
        if rpc.get_balance(&vault)? != 0 {
            return Err("test vault retains SOL after cleanup".into());
        }
        let auxiliary_accounts = state.route_auxiliary_accounts.clone();
        for (label, address) in auxiliary_accounts {
            let address = Pubkey::from_str(&address)?;
            let status = if rpc
                .get_account_with_commitment(&address, CommitmentConfig::finalized())?
                .value
                .is_some()
            {
                "retained_for_disposable_smart_account_reuse"
            } else {
                "closed_by_protocol"
            };
            state
                .route_auxiliary_cleanup
                .insert(label, status.to_owned());
        }
        save_state(state_path, state)?;
        Ok(())
    }

    fn remove_route_policy(
        rpc: &RpcClient,
        wallet: &Keypair,
        settings: Pubkey,
        label: &str,
        state_path: &Path,
        state: &mut MainnetState,
    ) -> Result<(), Box<dyn Error>> {
        let stage = format!("cleanup:remove-{label}-policy");
        let policy = route_policy_account(state, label)?;
        if state.cleanup.contains_key(&stage) {
            if rpc
                .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
                .value
                .is_some()
            {
                return Err(format!("removed {label} policy still exists").into());
            }
            return Ok(());
        }
        let instruction = remove_policy_instruction(settings, wallet.pubkey(), policy);
        let (transaction, blockhash, last_valid_block_height) =
            legacy_transaction(rpc, wallet, &[instruction])?;
        let (evidence, _) = send_route_stage(
            rpc,
            state_path,
            state,
            &stage,
            transaction,
            blockhash,
            last_valid_block_height,
            None,
        )?;
        if rpc
            .get_account_with_commitment(&policy, CommitmentConfig::finalized())?
            .value
            .is_some()
        {
            return Err(format!("removed {label} policy still exists").into());
        }
        state.cleanup.insert(stage, evidence);
        state.pending = None;
        save_state(state_path, state)?;
        Ok(())
    }

    fn completed_route_count(state: &MainnetState) -> usize {
        state
            .historical_routes
            .values()
            .filter(|route| {
                route.source_deposit.evidence.is_some()
                    && route.source_withdraw.evidence.is_some()
                    && route.swap.is_some()
                    && route.target_deposit.evidence.is_some()
                    && route.target_cleanup_withdraw.evidence.is_some()
            })
            .count()
    }
}
