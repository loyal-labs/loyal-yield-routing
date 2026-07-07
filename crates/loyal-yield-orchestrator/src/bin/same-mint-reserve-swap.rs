use std::process::Command;
use std::{
    collections::BTreeSet, convert::TryInto, env, error::Error, str::FromStr, thread,
    time::Duration,
};

use chrono::{DateTime, Utc};
use klend_interface::{
    discriminators::{
        DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2, INIT_OBLIGATION,
        REFRESH_OBLIGATION, WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2,
    },
    from_account_data,
    instructions::{
        deposit::{
            deposit_reserve_liquidity_and_obligation_collateral_v2,
            DepositReserveLiquidityAndObligationCollateralV2Accounts,
        },
        obligation::{init_obligation, InitObligationAccounts},
        refresh::{
            refresh_obligation, refresh_reserve, RefreshObligationAccounts, RefreshReserveAccounts,
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
    compile_squads_inner_instruction, create_init_obligation_yield_route_action,
    create_same_mint_market_mint_yield_route_action, derive_action_account,
    derive_kamino_obligation_farm_user_state, derive_kamino_vanilla_obligation,
    execute_program_interaction_policy_instruction, execute_sync_transaction_instruction,
    kamino_init_obligation_farm_instruction, remove_policy_instruction,
    update_all_in_one_market_mint_yield_route_action, update_init_obligation_yield_route_action,
    update_same_mint_market_mint_yield_route_action, KaminoInitObligationFarm, LoyalActionContext,
    RouteTopology, SwapLane, YieldRouteActionBuilder, YieldRouteActionSeeds, YieldRouteActionSetup,
    YieldRouteUniverse, ASSOCIATED_TOKEN_PROGRAM_ID, KAMINO_MAIN_USDC_RESERVE,
    SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_MINT, YIELD_ROUTE_WITHDRAW_ACTION_SEED,
};
use loyal_yield_orchestrator::sqlx::{
    postgres::{PgConnectOptions, PgPoolOptions},
    PgPool, Row,
};
use loyal_yield_orchestrator::{
    policy_keypair_from_env, route_amount_evidence_from_metadata, solana_testing_keypair_from_env,
    ConfirmSameMintRebalanceInput, CurrentIdleTokenBalance, DecisionAdvance, DecisionId,
    DecisionStatus, IdleVaultDepositDecisionInput, NeonSqlClient, PlanOutcomeStatus,
    PolicyMatchInput, RebalanceDecision, ReconciledReservePosition, ReconciledVaultState,
    RouteLookupTableUpsert, SameMintRebalanceInput, SameMintRebalanceResult, SnapshotId, VaultId,
    AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED, ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
};
use num_bigint::BigUint;
use num_traits::{ToPrimitive, Zero};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_client::rpc_client::RpcClient;
#[allow(deprecated)]
use solana_sdk::address_lookup_table::{
    instruction as address_lookup_table_instruction, program as address_lookup_table_program,
    state::{AddressLookupTable, LOOKUP_TABLE_MAX_ADDRESSES},
};
#[allow(deprecated)]
use solana_sdk::system_program;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0, AddressLookupTableAccount, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::Signer,
    transaction::VersionedTransaction,
};

const KAMINO_PRIME_USDC_RESERVE: &str = "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu";
const KAMINO_MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const KAMINO_PRIME_MARKET: &str = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA";
const KAMINO_MAPLE_MARKET: &str = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y";
const KAMINO_ONRE_MARKET: &str = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8";
const KAMINO_ETHENA_MARKET: &str = "BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF";
const SAME_MINT_ROUTE_MODE: &str = "same_mint_kamino";
const DEFAULT_SOLANA_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const PUBKEY_LEN: usize = 32;
const SQUADS_POLICY_ACCOUNT_DISCRIMINATOR: [u8; 8] = [222, 135, 7, 163, 235, 177, 33, 68];
const SPL_TOKEN_ACCOUNT_MINT_OFFSET: usize = 0;
const SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET: usize = 64;
const KAMINO_WITHDRAW_ROUTE_STEP: &str =
    "kamino_withdraw_obligation_collateral_and_redeem_reserve_collateral_v2";
const KAMINO_DEPOSIT_ROUTE_STEP: &str =
    "kamino_deposit_reserve_liquidity_and_obligation_collateral_v2";
const KAMINO_INIT_OBLIGATION_ROUTE_STEP: &str = "kamino_init_obligation";
const KAMINO_INIT_OBLIGATION_FARM_ROUTE_STEP: &str = "kamino_init_obligation_farms_for_reserve";
const KAMINO_REFRESH_OBLIGATION_ROUTE_STEP: &str = "kamino_refresh_obligation";
const KAMINO_STABLE_UNIVERSE_PRESET: &str = "kamino_stable";
const SAFE_RISK_PROFILE: &str = "safe";
const LOOKUP_TABLE_EXTEND_CHUNK_SIZE: usize = 20;
const LOOKUP_TABLE_WARMUP_MAX_POLLS: usize = 40;
const LOOKUP_TABLE_WARMUP_POLL_MS: u64 = 500;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Direction {
    MainToPrime,
    PrimeToMain,
}

impl Direction {
    fn parse(value: &str) -> Option<Self> {
        match value {
            "main-to-prime" => Some(Self::MainToPrime),
            "prime-to-main" => Some(Self::PrimeToMain),
            _ => None,
        }
    }

    fn as_str(self) -> &'static str {
        match self {
            Self::MainToPrime => "main-to-prime",
            Self::PrimeToMain => "prime-to-main",
        }
    }

    fn source_reserve(self) -> String {
        match self {
            Self::MainToPrime => KAMINO_MAIN_USDC_RESERVE.to_string(),
            Self::PrimeToMain => KAMINO_PRIME_USDC_RESERVE.to_owned(),
        }
    }

    fn target_reserve(self) -> String {
        match self {
            Self::MainToPrime => KAMINO_PRIME_USDC_RESERVE.to_owned(),
            Self::PrimeToMain => KAMINO_MAIN_USDC_RESERVE.to_string(),
        }
    }

    fn source_market(self) -> &'static str {
        match self {
            Self::MainToPrime => KAMINO_MAIN_MARKET,
            Self::PrimeToMain => KAMINO_PRIME_MARKET,
        }
    }

    fn target_market(self) -> &'static str {
        match self {
            Self::MainToPrime => KAMINO_PRIME_MARKET,
            Self::PrimeToMain => KAMINO_MAIN_MARKET,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ReserveMove {
    source_reserve: String,
    target_reserve: String,
}

impl ReserveMove {
    fn from_options(options: &CliOptions) -> Result<Self, String> {
        let (source_reserve, target_reserve) =
            match (&options.source_reserve, &options.target_reserve) {
                (Some(source), Some(target)) => (source.clone(), target.clone()),
                (None, None) => (
                    options.direction.source_reserve(),
                    options.direction.target_reserve(),
                ),
                _ => {
                    return Err(
                        "--source-reserve and --target-reserve must be provided together"
                            .to_owned(),
                    )
                }
            };
        Pubkey::from_str(&source_reserve)
            .map_err(|_| "--source-reserve must be a public key".to_owned())?;
        Pubkey::from_str(&target_reserve)
            .map_err(|_| "--target-reserve must be a public key".to_owned())?;
        if source_reserve == target_reserve {
            return Err("source and target reserves must be distinct".to_owned());
        }
        Ok(Self {
            source_reserve,
            target_reserve,
        })
    }
}

fn reconcile_reserves_for_move(options: &CliOptions, reserve_move: &ReserveMove) -> Vec<String> {
    let mut reserves = Vec::new();
    push_unique_string(&mut reserves, reserve_move.source_reserve.clone());
    push_unique_string(&mut reserves, reserve_move.target_reserve.clone());
    if options.full_withdraw_main_usdc {
        let main = KAMINO_MAIN_USDC_RESERVE.to_string();
        if !reserves.iter().any(|existing| existing == &main) {
            reserves.push(main);
        }
    }
    if let Some(reserve) = &options.full_withdraw_reserve {
        if !reserves.iter().any(|existing| existing == reserve) {
            reserves.push(reserve.clone());
        }
    }
    if let Some(reserve) = &options.initial_deposit_reserve {
        push_unique_string(&mut reserves, reserve.clone());
    }
    if let Some(reserve) = &options.idle_vault_deposit_reserve {
        push_unique_string(&mut reserves, reserve.clone());
    }
    if let Some(reserve) = &options.setup_obligation_reserve {
        if !reserves.iter().any(|existing| existing == reserve) {
            reserves.push(reserve.clone());
        }
    }
    for reserve in &options.reconcile_reserves {
        if !reserves.iter().any(|existing| existing == reserve) {
            reserves.push(reserve.clone());
        }
    }
    reserves
}

fn push_unique_string(values: &mut Vec<String>, value: String) {
    if !values.iter().any(|existing| existing == &value) {
        values.push(value);
    }
}

fn full_withdraw_reserve(options: &CliOptions) -> String {
    options
        .full_withdraw_reserve
        .clone()
        .unwrap_or_else(|| KAMINO_MAIN_USDC_RESERVE.to_string())
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CliOptions {
    settings: String,
    vault_index: i16,
    direction: Direction,
    source_reserve: Option<String>,
    target_reserve: Option<String>,
    update_policy: bool,
    update_active_policy: bool,
    initial_deposit_reserve: Option<String>,
    initial_deposit_amount_raw: Option<u64>,
    idle_vault_deposit_reserve: Option<String>,
    idle_vault_deposit_amount_raw: Option<u64>,
    full_withdraw_main_usdc: bool,
    full_withdraw_reserve: Option<String>,
    setup_obligation_reserve: Option<String>,
    e2e_deposit_amount_raw: Option<u64>,
    execute: bool,
    optimization_cycle: bool,
    reconcile_from_chain: bool,
    reconcile_current_positions: bool,
    reconcile_reserves: Vec<String>,
    seed_from_user_position: bool,
    provision_lookup_table: bool,
    provision_route_lookup_table: bool,
    expected_source_snapshot_id: Option<i64>,
    expected_liquidity_mint: Option<String>,
    expected_amount_raw: Option<i64>,
    expected_route_amount_semantics: Option<String>,
    expected_idle_token_account: Option<String>,
    expected_idle_observed_slot: Option<i64>,
    expected_idle_observed_at: Option<DateTime<Utc>>,
    expected_source_apy_bps: Option<i64>,
    expected_target_apy_bps: Option<i64>,
    expected_edge_bps: Option<i64>,
    rpc_url: String,
    lookup_tables: Vec<Pubkey>,
}

#[derive(Debug)]
struct SelectedVault {
    id: VaultId,
    settings: String,
    authority: String,
    policy_seed: i64,
    vault_index: i16,
    vault_pubkey: String,
    policy_account: String,
    setup_policy_account: Option<String>,
    setup_policy_seed: Option<i64>,
    delegated_signers: Vec<String>,
    threshold: i32,
    route_modes: Vec<String>,
    stable_mints: Vec<String>,
    kamino_markets: Vec<String>,
    kamino_liquidity_mints: Vec<String>,
    swap_lanes: Value,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PositionSummary {
    reserve: String,
    liquidity_mint: String,
    amount_raw: i64,
    has_value: bool,
    snapshot_id: SnapshotId,
    supply_apy_bps: Option<i64>,
    planning_metadata: Value,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ChainPositionSummary {
    reserve: String,
    market: String,
    liquidity_mint: String,
    liquidity_token_program: String,
    reserve_liquidity_supply: String,
    collateral_mint: String,
    reserve_collateral_supply: String,
    collateral_farm: Option<String>,
    collateral_farm_user_state: Option<String>,
    collateral_farm_user_state_exists: bool,
    pyth_oracle: Option<String>,
    switchboard_price_oracle: Option<String>,
    switchboard_twap_oracle: Option<String>,
    scope_prices: Option<String>,
    obligation: String,
    obligation_exists: bool,
    obligation_deposit_reserves: Vec<String>,
    obligation_borrow_reserves: Vec<String>,
    amount_raw: u64,
    redeemable_liquidity_amount_raw: u64,
    vault_liquidity_ata: String,
    vault_liquidity_token_account_exists: bool,
    vault_liquidity_amount_raw: u64,
}

#[derive(Clone, Debug)]
struct ChainReconcilePreview {
    observed_slot: i64,
    vault_user_metadata: String,
    vault_user_metadata_exists: bool,
    positions: Vec<ChainPositionSummary>,
}

#[derive(Debug)]
struct UserPositionSeedPreview {
    source: String,
    rows: Vec<UserPositionSeedRow>,
    positions: Vec<PositionSummary>,
}

#[derive(Debug)]
struct UserPositionSeedRow {
    id: i64,
    current_reserve: String,
    current_market: String,
    current_liquidity_mint: String,
    current_amount_raw: i64,
    current_observed_slot: i64,
    current_observed_at: Option<DateTime<Utc>>,
}

#[derive(Debug)]
struct PolicyAccountPreflight {
    policy_account: String,
    source_market: String,
    target_market: String,
    decoded: DecodedPolicyAccount,
}

impl PolicyAccountPreflight {
    fn allows_required_markets(&self) -> bool {
        self.decoded
            .kamino_markets
            .iter()
            .any(|market| market == &self.source_market)
            && self
                .decoded
                .kamino_markets
                .iter()
                .any(|market| market == &self.target_market)
    }

    fn allows_required_route_steps(&self) -> bool {
        self.decoded
            .instructions
            .iter()
            .any(|instruction| instruction.route_step == Some(KAMINO_WITHDRAW_ROUTE_STEP))
            && self
                .decoded
                .instructions
                .iter()
                .any(|instruction| instruction.route_step == Some(KAMINO_DEPOSIT_ROUTE_STEP))
    }

    fn allows_init_obligation(&self) -> bool {
        self.decoded
            .instructions
            .iter()
            .any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP))
    }

    fn allows_refresh_obligation(&self) -> bool {
        self.decoded
            .instructions
            .iter()
            .any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP))
    }
}

#[derive(Clone, Debug)]
struct DecodedPolicyAccount {
    layout: PolicyAccountLayout,
    delegated_signers: Vec<String>,
    threshold: u16,
    account_index: u8,
    instruction_count: usize,
    kamino_markets: Vec<String>,
    kamino_liquidity_mints: Vec<String>,
    constraints: Vec<PolicyInstructionConstraint>,
    instructions: Vec<DecodedPolicyInstructionSummary>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PolicyAccountLayout {
    ProgramInteractionPolicyState,
}

impl PolicyAccountLayout {
    fn as_str(self) -> &'static str {
        match self {
            Self::ProgramInteractionPolicyState => "program_interaction_policy_state",
        }
    }
}

#[derive(Clone, Debug)]
struct DecodedPolicyInstructionSummary {
    program_id: String,
    route_step: Option<&'static str>,
    data_discriminator: Option<Vec<u8>>,
    markets: Vec<String>,
    liquidity_mints: Vec<String>,
    account_constraints: Vec<DecodedPolicyAccountConstraintSummary>,
}

#[derive(Clone, Debug)]
struct DecodedPolicyAccountConstraintSummary {
    account_index: u8,
    kind: &'static str,
    pubkeys: Vec<String>,
    owner: Option<String>,
    data_constraints: Vec<DecodedPolicyDataConstraintSummary>,
}

#[derive(Clone, Debug)]
struct DecodedPolicyDataConstraintSummary {
    data_offset: u64,
    operator: &'static str,
    value: Value,
}

#[derive(Clone, Debug)]
struct PolicyInstructionConstraint {
    program_id: Pubkey,
    account_constraints: Vec<PolicyAccountConstraint>,
    data_constraints: Vec<PolicyDataConstraint>,
}

#[derive(Clone, Debug)]
struct PolicyAccountConstraint {
    account_index: u8,
    pubkeys: Vec<Pubkey>,
    data_constraints: Vec<PolicyDataConstraint>,
    owner: Option<Pubkey>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PolicyDataConstraint {
    data_offset: u64,
    data_value: PolicyDataValue,
    operator: PolicyDataOperator,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum PolicyDataValue {
    U8(u8),
    U16Le(u16),
    U32Le(u32),
    U64Le(u64),
    U128Le(u128),
    U8Slice(Vec<u8>),
}

impl PolicyDataValue {
    fn to_json(&self) -> Value {
        match self {
            Self::U8(value) => json!({ "kind": "u8", "value": value }),
            Self::U16Le(value) => json!({ "kind": "u16Le", "value": value }),
            Self::U32Le(value) => json!({ "kind": "u32Le", "value": value }),
            Self::U64Le(value) => json!({ "kind": "u64Le", "value": value.to_string() }),
            Self::U128Le(value) => json!({ "kind": "u128Le", "value": value.to_string() }),
            Self::U8Slice(value) => json!({ "kind": "u8Slice", "value": value }),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PolicyDataOperator {
    Equals,
    NotEquals,
    GreaterThan,
    GreaterThanOrEqualTo,
    LessThan,
    LessThanOrEqualTo,
}

impl PolicyDataOperator {
    fn as_str(self) -> &'static str {
        match self {
            Self::Equals => "equals",
            Self::NotEquals => "not_equals",
            Self::GreaterThan => "greater_than",
            Self::GreaterThanOrEqualTo => "greater_than_or_equal_to",
            Self::LessThan => "less_than",
            Self::LessThanOrEqualTo => "less_than_or_equal_to",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct InlineMissingObligationSetupPreview {
    target_obligation: String,
    target_reserve: String,
    target_market: String,
    policy_account: String,
    policy_source: &'static str,
    instruction_constraint_index: u8,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct RouteExecutionPreview {
    policy_account: String,
    setup_policy_account: Option<String>,
    fee_payer: String,
    signer: String,
    account_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    init_instruction_constraint_index: Option<u8>,
    policy_constraint_validation: Option<PolicyConstraintValidation>,
    missing_obligation_setup: Option<InlineMissingObligationSetupPreview>,
    setup_instruction_program: Option<String>,
    setup_instruction_discriminator: Option<Vec<u8>>,
    route_steps: Vec<&'static str>,
    refresh_reserves: Vec<String>,
    inner_instruction_count: usize,
    transaction_account_count: usize,
    outer_account_count: usize,
    source_instruction_program: String,
    target_instruction_program: String,
    source_instruction_discriminator: Vec<u8>,
    target_instruction_discriminator: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PolicyConstraintValidation {
    matches: bool,
    failures: Vec<String>,
}

#[derive(Clone, Debug)]
struct RouteExecutionPlan {
    pre_instructions: Vec<Instruction>,
    instructions: Vec<Instruction>,
    preview: RouteExecutionPreview,
}

#[derive(Debug)]
struct RouteExecutionSubmitResult {
    signature: String,
    submitted_slot: i64,
    confirmed_slot: i64,
    simulation_units_consumed: Option<u64>,
    transaction_packet: TransactionPacketSummary,
    lookup_table_provisioning: Value,
    confirmed: SameMintRebalanceResult,
}

#[derive(Debug)]
struct RouteLookupTableCoverage {
    scope: String,
    lookup_table_accounts: Vec<AddressLookupTableAccount>,
    required_addresses: Vec<Pubkey>,
    missing_addresses: Vec<Pubkey>,
}

impl RouteLookupTableCoverage {
    fn reuse_only_json(&self, options: &CliOptions, fee_payer: Pubkey) -> Value {
        json!({
            "enabled": false,
            "mode": "route_execution_reuse_only",
            "execute": options.execute,
            "status": if self.missing_addresses.is_empty() { "lookup_table_coverage_ready" } else { "lookup_table_coverage_missing" },
            "cluster": route_lookup_table_cluster(&options.rpc_url),
            "scope": self.scope.as_str(),
            "authority": fee_payer.to_string(),
            "payer": fee_payer.to_string(),
            "requiredAddresses": pubkeys_json(&self.required_addresses),
            "requiredAddressCount": self.required_addresses.len(),
            "missingBeforeProvision": pubkeys_json(&self.missing_addresses),
            "missingBeforeProvisionCount": self.missing_addresses.len(),
            "coverageAfterProvision": lookup_table_coverage_json(
                &self.required_addresses,
                &self.lookup_table_accounts
            ),
        })
    }
}

#[derive(Debug)]
struct MissingObligationSetupDryRun {
    policy_account: String,
    policy_source: &'static str,
    instruction_constraint_index: u8,
    init_execution: PolicyTransactionBuild,
}

#[derive(Debug)]
struct MissingObligationSetupSubmitResult {
    policy_account: String,
    policy_source: &'static str,
    instruction_constraint_index: u8,
    init_signature: String,
    init_submitted_slot: i64,
    init_confirmed_slot: i64,
    init_simulation_units_consumed: Option<u64>,
    init_transaction_packet: TransactionPacketSummary,
}

#[derive(Debug)]
struct SubmittedPolicyTransaction {
    signature: String,
    submitted_slot: i64,
    confirmed_slot: i64,
}

#[derive(Debug)]
struct InitialDepositSubmitResult {
    funding_signature: Option<String>,
    funding_submitted_slot: Option<i64>,
    funding_confirmed_slot: Option<i64>,
    funding_simulation_units_consumed: Option<u64>,
    funding_transaction_packet: TransactionPacketSummary,
    policy_signature: Option<String>,
    policy_submitted_slot: Option<i64>,
    policy_confirmed_slot: Option<i64>,
    policy_simulation_units_consumed: Option<u64>,
    policy_transaction_packet: TransactionPacketSummary,
    reconciled_snapshot_id: Option<SnapshotId>,
    post_chain_preview: Option<ChainReconcilePreview>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct InitialDepositPolicyPreview {
    policy_account: String,
    signer: String,
    account_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    policy_constraint_validation: Option<PolicyConstraintValidation>,
    setup_instruction_program: Option<String>,
    setup_instruction_discriminator: Option<Vec<u8>>,
    route_steps: Vec<&'static str>,
    inner_instruction_count: usize,
    transaction_account_count: usize,
    outer_account_count: usize,
    deposit_instruction_program: String,
    deposit_instruction_discriminator: Vec<u8>,
}

#[derive(Clone, Debug)]
struct InitialDepositPolicyPlan {
    pre_instructions: Vec<Instruction>,
    instruction: Instruction,
    preview: InitialDepositPolicyPreview,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct FullWithdrawPolicyPreview {
    policy_account: String,
    signer: String,
    account_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    policy_constraint_validation: Option<PolicyConstraintValidation>,
    route_steps: Vec<&'static str>,
    inner_instruction_count: usize,
    transaction_account_count: usize,
    outer_account_count: usize,
    withdraw_instruction_program: String,
    withdraw_instruction_discriminator: Vec<u8>,
}

#[derive(Clone, Debug)]
struct FullWithdrawPolicyPlan {
    pre_instructions: Vec<Instruction>,
    instruction: Instruction,
    preview: FullWithdrawPolicyPreview,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct AccountProof {
    pubkey: String,
    exists: bool,
    lamports: u64,
    owner: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct ObligationAccountProof {
    account: AccountProof,
    owner: Option<String>,
    lending_market: Option<String>,
    active_deposit_count: Option<usize>,
    active_borrow_count: Option<usize>,
    reserve_deposited_amount_raw: Option<u64>,
}

#[derive(Debug)]
struct PolicyTransactionBuild {
    transaction: VersionedTransaction,
    transaction_packet: TransactionPacketSummary,
    best_case_single_lookup_table_packet: Option<TransactionPacketSummary>,
    simulation_error: Option<String>,
    simulation_logs: Value,
    simulation_skipped_reason: Option<String>,
    simulation_units_consumed: Option<u64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum AltInstructionMode {
    RejectProvisioning,
    AllowProvisioning,
}

#[derive(Debug)]
struct TransactionPacketSummary {
    version: &'static str,
    fee_payer: String,
    signer_pubkeys: Vec<String>,
    packet_size_bytes: usize,
    packet_data_size_bytes: usize,
    fits_packet_data_size: bool,
    static_account_key_count: usize,
    address_table_lookup_count: usize,
    loaded_writable_address_count: usize,
    loaded_readonly_address_count: usize,
    compiled_instruction_count: usize,
    instruction_data_bytes: usize,
    lookup_table_accounts: Vec<LookupTableAccountSummary>,
}

#[derive(Debug)]
struct LookupTableAccountSummary {
    account: String,
    address_count: usize,
    addresses: Option<Vec<String>>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct PreparedSameMintDecision {
    id: DecisionId,
    vault_id: VaultId,
    source_snapshot_id: SnapshotId,
    source_reserve: String,
    target_reserve: String,
    liquidity_mint: String,
    source_liquidity_mint: String,
    target_liquidity_mint: String,
    amount_raw: i64,
    source_apy_bps: i64,
    target_apy_bps: i64,
    estimated_edge_bps: i64,
    estimated_cost_lamports: i64,
    execution_plan: Value,
    idempotency_key: String,
}

#[derive(Debug, PartialEq, Eq)]
enum PlanBlocker {
    MissingCurrentPosition,
    MissingSourceReserve(String),
    MissingTargetReserve(String),
    SourceHasNoValue,
    TargetMintMismatch {
        actual: String,
        expected: String,
    },
    UnsupportedAmountSemantics {
        reserve: String,
        amount_semantics: Option<String>,
    },
    MonitorPlanDrift(String),
    ActiveDecision {
        decision_id: i64,
        status: String,
    },
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let options = match parse_args(env::args().skip(1)) {
        Ok(value) => value,
        Err(message) if message == "help" => {
            print_help();
            return Ok(());
        }
        Err(message) => return Err(message.into()),
    };
    let reserve_move = if let Some(reserve) = &options.idle_vault_deposit_reserve {
        ReserveMove {
            source_reserve: reserve.clone(),
            target_reserve: reserve.clone(),
        }
    } else {
        ReserveMove::from_options(&options)?
    };
    let database_url =
        env::var("NEON_DATABASE_URL").map_err(|_| "NEON_DATABASE_URL must be set")?;
    let pool = connect(&database_url).await?;
    let client = NeonSqlClient::from_pool(pool.clone());
    client.apply_migrations().await?;

    if let Some(amount_raw) = options.e2e_deposit_amount_raw {
        run_lifecycle_e2e_flow(&options, amount_raw)?;
        return Ok(());
    }

    if options.update_policy {
        let default_authority = solana_testing_keypair_from_env()?.pubkey();
        let default_delegated_signer = policy_keypair_from_env()?.pubkey();
        let vault = if options.update_active_policy {
            match load_active_vault(&pool, &options.settings, options.vault_index).await? {
                Some(vault) => vault,
                None => load_policy_target_vault(
                    &pool,
                    &options.settings,
                    options.vault_index,
                    default_authority,
                    default_delegated_signer,
                )
                .await?
                .ok_or("no managed vault found for settings and vault index")?,
            }
        } else {
            load_policy_target_vault(
                &pool,
                &options.settings,
                options.vault_index,
                default_authority,
                default_delegated_signer,
            )
            .await?
            .ok_or("no managed vault found for settings and vault index")?
        };
        validate_vault_policy(&vault)?;
        run_policy_update_flow(&options, &client, &vault).await?;
        return Ok(());
    }

    let vault = load_active_vault(&pool, &options.settings, options.vault_index)
        .await?
        .ok_or("no active managed vault found for settings and vault index")?;
    validate_vault_policy(&vault)?;
    let reconcile_reserves = reconcile_reserves_for_move(&options, &reserve_move);

    let requires_chain_preview = options.reconcile_from_chain
        || options.initial_deposit_amount_raw.is_some()
        || options.idle_vault_deposit_amount_raw.is_some()
        || options.full_withdraw_main_usdc
        || options.full_withdraw_reserve.is_some()
        || options.setup_obligation_reserve.is_some()
        || options.reconcile_current_positions;
    let chain_preview = if requires_chain_preview {
        Some(load_chain_reconcile_preview(
            &options.rpc_url,
            &vault,
            &reconcile_reserves,
        )?)
    } else {
        None
    };
    let policy_preflight = if let Some(preview) = &chain_preview {
        Some(load_policy_account_preflight(
            &options.rpc_url,
            &vault,
            preview,
            &reserve_move,
        )?)
    } else {
        None
    };
    if options.reconcile_current_positions {
        run_reconcile_current_positions_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("reconcile current positions requires chain preview")?,
        )
        .await?;
        return Ok(());
    }
    if let Some(amount_raw) = options.idle_vault_deposit_amount_raw {
        let deposit_reserve = options
            .idle_vault_deposit_reserve
            .as_deref()
            .ok_or("idle vault deposit reserve is required")?;
        run_idle_vault_deposit_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("idle vault deposit requires chain preview")?,
            policy_preflight.as_ref(),
            deposit_reserve,
            amount_raw,
        )
        .await?;
        return Ok(());
    }
    if let Some(amount_raw) = options.initial_deposit_amount_raw {
        let deposit_reserve = options
            .initial_deposit_reserve
            .clone()
            .unwrap_or_else(|| KAMINO_MAIN_USDC_RESERVE.to_string());
        run_initial_reserve_deposit_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("initial deposit requires chain preview")?,
            policy_preflight.as_ref(),
            &deposit_reserve,
            amount_raw,
        )
        .await?;
        return Ok(());
    }
    if let Some(setup_reserve) = &options.setup_obligation_reserve {
        run_setup_obligation_flow(
            &options,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("setup obligation requires chain preview")?,
            setup_reserve,
            policy_preflight.as_ref(),
        )
        .await?;
        return Ok(());
    }
    if options.full_withdraw_main_usdc || options.full_withdraw_reserve.is_some() {
        let withdraw_reserve = full_withdraw_reserve(&options);
        run_full_reserve_withdraw_flow(
            &options,
            &client,
            &vault,
            chain_preview
                .as_ref()
                .ok_or("full reserve withdraw requires chain preview")?,
            policy_preflight.as_ref(),
            &withdraw_reserve,
        )
        .await?;
        return Ok(());
    }
    if options.execute {
        if let Some(reason) = execution_preflight_blocker(
            chain_preview.as_ref(),
            policy_preflight.as_ref(),
            &reserve_move,
            None,
        ) {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "execution_preflight_blocked",
                    "reason": reason,
                    "writesDecision": false,
                    "writesCurrentPositions": false,
                    "picksUpExecution": false,
                    "sendsTransactions": false,
                    "direction": options.direction.as_str(),
                    "vault": vault_json(&vault),
                    "requiredReserves": required_reserves_json(&reserve_move),
                    "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                    "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                    "targetObligationSetup": chain_preview.as_ref().and_then(|preview| target_obligation_setup_json(preview, &reserve_move, &vault, policy_preflight.as_ref())),
                    "missingObligationSetup": Value::Null,
                }))?
            );
            return Err("same-mint execution preflight blocked before DB writes".into());
        }
    }
    let mut db_positions = load_position_summaries(&client, vault.id).await?;
    let user_position_seed = if options.seed_from_user_position {
        load_user_position_seed_preview(
            &pool,
            &vault,
            &reserve_move,
            chain_preview.as_ref(),
            options.direction,
        )
        .await?
    } else {
        None
    };
    let mut reconciled_snapshot_id = None;
    let should_write_current_positions_from_chain = writes_current_positions_from_chain(&options);
    let should_write_current_positions_from_user_seed =
        writes_current_positions_from_user_seed(&options);
    if should_write_current_positions_from_chain {
        let preview = chain_preview
            .as_ref()
            .ok_or("--execute requires --reconcile-from-chain")?;
        let state = chain_preview_reconciled_state(preview)?;
        let snapshot = client.reconcile_vault(vault.id, state).await?;
        reconciled_snapshot_id = Some(snapshot.id);
        db_positions = load_position_summaries(&client, vault.id).await?;
    } else if should_write_current_positions_from_user_seed {
        let seed = user_position_seed
            .as_ref()
            .ok_or("no active user_yield_positions row found for selected vault")?;
        let target_market = target_market_for_seed(
            seed,
            &reserve_move,
            chain_preview.as_ref(),
            options.direction,
        )?;
        let state = user_position_seed_reconciled_state(seed, &reserve_move, &target_market)?;
        let snapshot = client.reconcile_vault(vault.id, state).await?;
        reconciled_snapshot_id = Some(snapshot.id);
        db_positions = load_position_summaries(&client, vault.id).await?;
    }

    let using_chain_preview_positions =
        uses_chain_preview_positions(&options, chain_preview.is_some());
    let using_seed_preview_positions =
        !options.execute && user_position_seed.is_some() && !using_chain_preview_positions;
    let current_positions_source = if should_write_current_positions_from_chain {
        "vault_reserve_positions_current_after_chain_reconcile"
    } else if should_write_current_positions_from_user_seed {
        "vault_reserve_positions_current_after_user_position_seed"
    } else if using_chain_preview_positions {
        "chain_reconcile_preview"
    } else if using_seed_preview_positions {
        "user_yield_positions_seed_preview"
    } else {
        "neon_current_positions"
    };
    let pre_reconcile_positions = if using_chain_preview_positions {
        chain_preview
            .as_ref()
            .map(preview_position_summaries)
            .unwrap_or_default()
    } else if using_seed_preview_positions {
        let seed = user_position_seed
            .as_ref()
            .expect("using seed preview implies seed exists");
        seed.positions.clone()
    } else {
        db_positions.clone()
    };
    let active_decision = load_active_decision(&pool, vault.id).await?;

    let pre_reconcile_input = match build_same_mint_input(
        &options,
        &reserve_move,
        vault.id,
        &pre_reconcile_positions,
        active_decision,
    ) {
        Ok(value) => value,
        Err(blocker) => {
            let report = blocker_report(
                &options,
                &reserve_move,
                &vault,
                &db_positions,
                chain_preview.as_ref(),
                policy_preflight.as_ref(),
                user_position_seed.as_ref(),
                reconciled_snapshot_id,
                blocker,
            );
            println!("{}", serde_json::to_string_pretty(&report)?);
            if options.execute {
                return Err(
                    "same-mint execution prerequisite failed before DB command write".into(),
                );
            }
            return Ok(());
        }
    };
    let route_fee_payer = if chain_preview.is_some() {
        Some(same_mint_route_fee_payer_pubkey(&options)?)
    } else {
        None
    };
    let (route_execution, route_build_error) = if let Some(preview) = &chain_preview {
        let route_rpc = RpcClient::new_with_commitment(
            options.rpc_url.to_owned(),
            CommitmentConfig::confirmed(),
        );
        match build_route_execution_plan(
            Some(&route_rpc),
            &vault,
            preview,
            &reserve_move,
            &pre_reconcile_input,
            policy_preflight.as_ref(),
            route_fee_payer.expect("chain preview implies route fee payer"),
        ) {
            Ok(plan) => (Some(plan), None),
            Err(error) if !options.execute => (None, Some(error.to_string())),
            Err(error) => return Err(error),
        }
    } else {
        (None, None)
    };
    let inline_missing_obligation_setup = route_execution
        .as_ref()
        .and_then(|plan| plan.preview.missing_obligation_setup.as_ref())
        .map(inline_missing_obligation_setup_json);
    let mut execution_preflight_blockers = execution_preflight_blockers(
        chain_preview.as_ref(),
        policy_preflight.as_ref(),
        &reserve_move,
        route_execution.as_ref(),
    );
    if let Some(error) = &route_build_error {
        execution_preflight_blockers
            .push(format!("route execution plan could not be built: {error}"));
    }
    let mut route_lookup_table_provisioning: Option<Value> = None;
    if let Some(route_execution) = &route_execution {
        let mut transaction_instructions = route_execution.pre_instructions.clone();
        transaction_instructions.extend(route_execution.instructions.iter().cloned());
        if let Err(error) = guard_lookup_table_mutations(
            &transaction_instructions,
            AltInstructionMode::RejectProvisioning,
            "route execution",
        ) {
            execution_preflight_blockers.push(error.to_string());
        }
        let fee_payer = Pubkey::from_str(&route_execution.preview.fee_payer)?;
        let delegated_signer = Pubkey::from_str(&route_execution.preview.signer)?;
        let lookup_table_scope = same_mint_route_lookup_table_scope(&vault, &reserve_move);
        let route_rpc = RpcClient::new_with_commitment(
            options.rpc_url.to_owned(),
            CommitmentConfig::confirmed(),
        );
        let coverage = route_lookup_table_reuse_coverage(
            &client,
            &route_rpc,
            &options,
            &lookup_table_scope,
            fee_payer,
            delegated_signer,
            route_execution,
        )
        .await?;
        if !options.provision_route_lookup_table {
            if let Err(error) =
                ensure_route_lookup_table_coverage(&coverage.scope, &coverage.missing_addresses)
            {
                execution_preflight_blockers.push(error.to_string());
            }
        }
        route_lookup_table_provisioning = Some(coverage.reuse_only_json(&options, fee_payer));
    }
    let execution_preflight_blocker_reason = execution_preflight_blockers.first().cloned();
    let would_execute_route =
        route_execution.is_some() && execution_preflight_blocker_reason.is_none();
    if options.provision_route_lookup_table {
        if let Some(reason) = &execution_preflight_blocker_reason {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "route_lookup_table_provisioning_blocked",
                    "reason": reason,
                    "writesDecision": false,
                    "writesCurrentPositions": false,
                    "picksUpExecution": false,
                    "sendsTransactions": false,
                    "direction": options.direction.as_str(),
                    "vault": vault_json(&vault),
                    "requiredReserves": required_reserves_json(&reserve_move),
                    "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                    "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                    "routeExecution": route_execution.as_ref().map(|plan| route_execution_preview_json(&plan.preview)),
                }))?
            );
            return Err("route lookup table provisioning blocked before send".into());
        }
        let route_execution = route_execution
            .as_ref()
            .ok_or("route execution plan is unavailable for lookup table provisioning")?;
        let provisioning = provision_same_mint_route_lookup_table(
            &client,
            &options,
            &vault,
            &reserve_move,
            route_execution,
        )
        .await?;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": if options.execute { "route_lookup_table_provisioned" } else { "route_lookup_table_provisioning_dry_run" },
                "writesDecision": false,
                "writesCurrentPositions": false,
                "picksUpExecution": false,
                "sendsTransactions": options.execute,
                "direction": options.direction.as_str(),
                "vault": vault_json(&vault),
                "requiredReserves": required_reserves_json(&reserve_move),
                "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                "routeExecution": route_execution_preview_json(&route_execution.preview),
                "lookupTableProvisioning": provisioning,
            }))?
        );
        return Ok(());
    }
    if options.execute {
        if let Some(reason) = &execution_preflight_blocker_reason {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "execution_preflight_blocked",
                    "reason": reason,
                    "writesDecision": false,
                    "writesCurrentPositions": options.reconcile_from_chain,
                    "picksUpExecution": false,
                    "sendsTransactions": false,
                    "direction": options.direction.as_str(),
                    "vault": vault_json(&vault),
                    "requiredReserves": required_reserves_json(&reserve_move),
                    "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
                    "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                    "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
                    "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                    "sameMintInput": same_mint_input_json(&pre_reconcile_input),
                    "routeExecution": route_execution.as_ref().map(|plan| route_execution_preview_json(&plan.preview)),
                    "lookupTableProvisioning": route_lookup_table_provisioning.clone(),
                    "targetObligationSetup": chain_preview.as_ref().and_then(|preview| target_obligation_setup_json(preview, &reserve_move, &vault, policy_preflight.as_ref())),
                    "missingObligationSetup": inline_missing_obligation_setup.clone(),
                }))?
            );
            return Err("same-mint execution preflight blocked before decision write".into());
        }
    }

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "dry_run",
                "writesDecision": false,
                "wouldWriteDecision": execution_preflight_blocker_reason.is_none(),
                "wouldBuildRoute": route_execution.is_some(),
                "wouldExecuteRoute": would_execute_route,
                "executionPreflightBlocker": execution_preflight_blocker_reason,
                "executionPreflightBlockers": execution_preflight_blockers,
                "wouldReconcileCurrentPositions": options.reconcile_from_chain,
                "wouldSeedCurrentPositions": options.seed_from_user_position,
                "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
                "currentPositionsSource": current_positions_source,
                "direction": options.direction.as_str(),
                "vault": vault_json(&vault),
                "requiredReserves": required_reserves_json(&reserve_move),
                "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
                "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
                "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                "sameMintInput": same_mint_input_json(&pre_reconcile_input),
                "routeBuildError": route_build_error,
                "routeExecution": route_execution.as_ref().map(|plan| route_execution_preview_json(&plan.preview)),
                "lookupTableProvisioning": route_lookup_table_provisioning,
                "targetObligationSetup": chain_preview.as_ref().and_then(|preview| target_obligation_setup_json(preview, &reserve_move, &vault, policy_preflight.as_ref())),
                "missingObligationSetup": inline_missing_obligation_setup.clone(),
                "executionPlan": {
                    "kind": "same_mint",
                    "routeSteps": route_execution.as_ref().map(|plan| plan.preview.route_steps.clone()).unwrap_or_else(|| vec![KAMINO_WITHDRAW_ROUTE_STEP, KAMINO_DEPOSIT_ROUTE_STEP]),
                    "policyExecutions": route_execution.as_ref().map(|plan| plan.preview.route_steps.len()).unwrap_or(1)
                }
            }))?
        );
        return Ok(());
    }

    let prepared = client
        .prepare_same_mint_rebalance(pre_reconcile_input.clone())
        .await?;
    if prepared.status == DecisionStatus::Planned {
        let decision_id = prepared
            .decision_id
            .ok_or("planned same-mint rebalance result did not include decision id")?;
        let execution_decision = match load_prepared_same_mint_decision(&pool, decision_id).await {
            Ok(value) => value,
            Err(error) => {
                let reason = error.to_string();
                let _ = client
                    .advance_decision(
                        decision_id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await;
                return Err(format!(
                    "same-mint route execution failed after decision {}: {reason}",
                    decision_id.as_i64()
                )
                .into());
            }
        };
        if let Err(error) = validate_execution_decision_route(&execution_decision, &reserve_move) {
            let reason = error.to_string();
            let _ = client
                .advance_decision(
                    decision_id,
                    DecisionAdvance::Fail {
                        reason: reason.clone(),
                    },
                )
                .await;
            return Err(format!(
                "same-mint route execution failed after decision {}: {reason}",
                decision_id.as_i64()
            )
            .into());
        }
        let execution_input = same_mint_input_from_decision(&execution_decision);
        let chain_reconcile = chain_preview
            .as_ref()
            .ok_or("--execute requires --reconcile-from-chain route execution plan")?;
        let route_rpc = RpcClient::new_with_commitment(
            options.rpc_url.to_owned(),
            CommitmentConfig::confirmed(),
        );
        let route_execution = match build_route_execution_plan(
            Some(&route_rpc),
            &vault,
            chain_reconcile,
            &reserve_move,
            &execution_input,
            policy_preflight.as_ref(),
            same_mint_route_fee_payer_pubkey(&options)?,
        ) {
            Ok(value) => value,
            Err(error) => {
                let reason = error.to_string();
                let _ = client
                    .advance_decision(
                        decision_id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await;
                return Err(format!(
                    "same-mint route execution failed after decision {}: {reason}",
                    decision_id.as_i64()
                )
                .into());
            }
        };
        let execution = match execute_prepared_same_mint_route(
            &client,
            &options,
            &vault,
            &execution_decision,
            &route_execution,
        )
        .await
        {
            Ok(value) => value,
            Err(error) => {
                let reason = error.to_string();
                let _ = client
                    .advance_decision(
                        decision_id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await;
                return Err(format!(
                    "same-mint route execution failed after decision {}: {reason}",
                    decision_id.as_i64()
                )
                .into());
            }
        };
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "executed",
                "writesDecision": true,
                "picksUpExecution": true,
                "sendsTransactions": true,
                "wouldReconcileCurrentPositions": options.reconcile_from_chain,
                "wouldSeedCurrentPositions": options.seed_from_user_position,
                "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
                "currentPositionsSource": current_positions_source,
                "direction": options.direction.as_str(),
                "vault": vault_json(&vault),
                "requiredReserves": required_reserves_json(&reserve_move),
                "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
                "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
                "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
                "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
                "sameMintInput": same_mint_input_json(&pre_reconcile_input),
                "preparedDecision": same_mint_result_json(&prepared),
                "executionDecision": prepared_same_mint_decision_json(&execution_decision),
                "routeExecution": route_execution_preview_json(&route_execution.preview),
                "missingObligationSetup": route_execution.preview.missing_obligation_setup.as_ref().map(inline_missing_obligation_setup_json),
                "executionPickup": {
                    "decisionId": decision_id.as_i64(),
                    "source": "loyal_yield.rebalance_decisions",
                    "signature": execution.signature,
                    "submittedSlot": execution.submitted_slot,
                    "confirmedSlot": execution.confirmed_slot,
                    "simulationUnitsConsumed": execution.simulation_units_consumed,
                    "transaction": transaction_packet_json(&execution.transaction_packet),
                    "lookupTableProvisioning": execution.lookup_table_provisioning,
                    "finalStatus": execution.confirmed.status.as_str(),
                },
                "confirmedDecision": same_mint_result_json(&execution.confirmed),
                "executionPlan": {
                    "kind": "same_mint",
                    "routeSteps": route_execution.preview.route_steps.clone(),
                    "policyExecutions": route_execution.preview.route_steps.len()
                }
            }))?
        );
        return Ok(());
    }

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "prepare_same_mint_rebalance_did_not_plan",
            "writesDecision": prepared.decision_id.is_some(),
            "picksUpExecution": false,
            "sendsTransactions": false,
            "wouldReconcileCurrentPositions": options.reconcile_from_chain,
            "wouldSeedCurrentPositions": options.seed_from_user_position,
            "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
            "currentPositionsSource": current_positions_source,
            "direction": options.direction.as_str(),
            "vault": vault_json(&vault),
            "requiredReserves": required_reserves_json(&reserve_move),
            "currentPositions": db_positions.iter().map(position_json).collect::<Vec<_>>(),
            "chainReconcile": chain_preview.as_ref().map(chain_reconcile_preview_json),
            "userPositionSeed": user_position_seed.as_ref().map(user_position_seed_preview_json),
            "policyPreflight": policy_route_preflight_json(&vault, &reserve_move, policy_preflight.as_ref()),
            "sameMintInput": same_mint_input_json(&pre_reconcile_input),
            "preparedDecision": same_mint_result_json(&prepared),
            "routeExecution": route_execution.as_ref().map(|plan| route_execution_preview_json(&plan.preview)),
            "missingObligationSetup": inline_missing_obligation_setup.clone(),
        }))?
    );
    Err("same-mint rebalance was not planned".into())
}

async fn provision_same_mint_route_lookup_table(
    client: &NeonSqlClient,
    options: &CliOptions,
    vault: &SelectedVault,
    reserve_move: &ReserveMove,
    route_execution: &RouteExecutionPlan,
) -> Result<Value, Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let signer = policy_keypair_from_env()?;
    let fee_payer: &dyn Signer = &signer;
    let expected_fee_payer = Pubkey::from_str(&route_execution.preview.fee_payer)?;
    if fee_payer.pubkey() != expected_fee_payer {
        return Err(format!(
            "route ALT provisioning payer {} does not match prepared route fee payer {}",
            fee_payer.pubkey(),
            expected_fee_payer
        )
        .into());
    }
    let scope = same_mint_route_lookup_table_scope(vault, reserve_move);
    let lookup_table_pubkeys =
        lookup_table_pubkeys_for_scope(client, options, &scope, fee_payer.pubkey()).await?;
    let mut lookup_table_accounts =
        load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let mut transaction_instructions = route_execution.pre_instructions.clone();
    transaction_instructions.extend(route_execution.instructions.iter().cloned());
    let signer_pubkeys = same_mint_route_signer_pubkeys(fee_payer.pubkey(), signer.pubkey());
    let required_lookup_addresses = best_case_lookup_table_addresses(
        fee_payer.pubkey(),
        &transaction_instructions,
        &signer_pubkeys,
    );
    prepare_durable_route_lookup_table(
        client,
        &rpc,
        options,
        &scope,
        fee_payer.pubkey(),
        fee_payer,
        &required_lookup_addresses,
        &mut lookup_table_accounts,
    )
    .await
}

fn run_lifecycle_e2e_flow(options: &CliOptions, amount_raw: u64) -> Result<(), Box<dyn Error>> {
    let phase_specs = lifecycle_e2e_phase_specs(amount_raw);
    let mut phase_results = Vec::new();
    let mut runtime_lookup_tables = Vec::new();
    for spec in phase_specs {
        let phase = LifecyclePhaseCommand {
            name: spec.name,
            args: lifecycle_phase_args(options, &spec.args, &runtime_lookup_tables),
        };
        let result = run_lifecycle_phase(&phase, options)?;
        let success = result
            .get("process")
            .and_then(|process| process.get("success"))
            .and_then(Value::as_bool)
            .unwrap_or(false);
        phase_results.push(result);
        if options.execute && !success {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "lifecycle_e2e_phase_failed",
                    "writesDecision": options.execute,
                    "sendsTransactions": options.execute,
                    "execute": options.execute,
                    "settings": options.settings,
                    "vaultIndex": options.vault_index,
                    "depositAmountRaw": amount_raw.to_string(),
                    "runtimeLookupTables": runtime_lookup_tables.iter().map(ToString::to_string).collect::<Vec<_>>(),
                    "phases": phase_results,
                }))?
            );
            return Err("same-mint lifecycle E2E phase failed".into());
        }
        if options.execute && spec.name == "policy_update" {
            if let Some(created_lookup_table) =
                created_lookup_table_from_lifecycle_phase_result(phase_results.last().unwrap())?
            {
                runtime_lookup_tables.push(created_lookup_table);
            }
        }
    }
    let all_phase_processes_succeeded = phase_results.iter().all(|result| {
        result
            .get("process")
            .and_then(|process| process.get("success"))
            .and_then(Value::as_bool)
            .unwrap_or(false)
    });

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": if options.execute { "lifecycle_e2e_executed" } else { "lifecycle_e2e_dry_run" },
            "writesDecision": options.execute,
            "sendsTransactions": options.execute,
            "execute": options.execute,
            "allPhaseProcessesSucceeded": all_phase_processes_succeeded,
            "settings": options.settings,
            "vaultIndex": options.vault_index,
            "depositAmountRaw": amount_raw.to_string(),
            "runtimeLookupTables": runtime_lookup_tables.iter().map(ToString::to_string).collect::<Vec<_>>(),
            "phaseOrder": [
                "policy_update",
                "initial_main_usdc_deposit",
                "move_main_to_prime",
                "move_prime_to_main",
                "full_main_usdc_withdraw"
            ],
            "phases": phase_results,
        }))?
    );
    Ok(())
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct LifecyclePhaseCommand {
    name: &'static str,
    args: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct LifecyclePhaseSpec {
    name: &'static str,
    args: Vec<String>,
}

fn lifecycle_e2e_phase_specs(amount_raw: u64) -> Vec<LifecyclePhaseSpec> {
    vec![
        LifecyclePhaseSpec {
            name: "policy_update",
            args: vec![
                "--update-policy".to_owned(),
                "--provision-lookup-table".to_owned(),
            ],
        },
        LifecyclePhaseSpec {
            name: "initial_main_usdc_deposit",
            args: vec!["--deposit-main-usdc".to_owned(), amount_raw.to_string()],
        },
        LifecyclePhaseSpec {
            name: "move_main_to_prime",
            args: vec![
                "--direction".to_owned(),
                "main-to-prime".to_owned(),
                "--reconcile-from-chain".to_owned(),
            ],
        },
        LifecyclePhaseSpec {
            name: "move_prime_to_main",
            args: vec![
                "--direction".to_owned(),
                "prime-to-main".to_owned(),
                "--reconcile-from-chain".to_owned(),
            ],
        },
        LifecyclePhaseSpec {
            name: "full_main_usdc_withdraw",
            args: vec!["--full-withdraw-main-usdc".to_owned()],
        },
    ]
}

fn lifecycle_phase_args(
    options: &CliOptions,
    phase_args: &[String],
    runtime_lookup_tables: &[Pubkey],
) -> Vec<String> {
    let mut args = vec![
        "--settings".to_owned(),
        options.settings.clone(),
        "--vault-index".to_owned(),
        options.vault_index.to_string(),
    ];
    for lookup_table in &options.lookup_tables {
        args.extend(["--lookup-table".to_owned(), lookup_table.to_string()]);
    }
    for lookup_table in runtime_lookup_tables {
        args.extend(["--lookup-table".to_owned(), lookup_table.to_string()]);
    }
    args.extend(phase_args.iter().cloned());
    if options.seed_from_user_position {
        args.push("--seed-from-user-position".to_owned());
    }
    if options.execute {
        args.push("--execute".to_owned());
    }
    args
}

fn created_lookup_table_from_lifecycle_phase_result(
    result: &Value,
) -> Result<Option<Pubkey>, Box<dyn Error>> {
    let Some(raw) = result
        .pointer("/stdout/lookupTableProvisioning/createdLookupTable")
        .and_then(Value::as_str)
    else {
        return Ok(None);
    };
    Pubkey::from_str(raw).map(Some).map_err(|error| {
        format!("policy update returned invalid lookup table {raw}: {error}").into()
    })
}

fn run_lifecycle_phase(
    phase: &LifecyclePhaseCommand,
    options: &CliOptions,
) -> Result<Value, Box<dyn Error>> {
    let exe = env::current_exe()?;
    let output = Command::new(exe)
        .args(&phase.args)
        .env("SOLANA_RPC_URL", &options.rpc_url)
        .output()?;
    let stdout = String::from_utf8_lossy(&output.stdout).trim().to_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).trim().to_owned();
    let parsed_stdout = if stdout.is_empty() {
        Value::Null
    } else {
        serde_json::from_str(&stdout).unwrap_or_else(|_| json!({ "raw": stdout }))
    };
    Ok(json!({
        "name": phase.name,
        "args": phase.args,
        "process": {
            "success": output.status.success(),
            "code": output.status.code(),
        },
        "stdout": parsed_stdout,
        "stderr": if stderr.is_empty() { Value::Null } else { json!(stderr) },
    }))
}

async fn run_policy_update_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
) -> Result<(), Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let settings = Pubkey::from_str(&vault.settings)?;
    let authority = Pubkey::from_str(&vault.authority)?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let policy = Pubkey::from_str(&vault.policy_account)?;
    let policy_seed = u64::try_from(vault.policy_seed).map_err(|_| "policy_seed must be >= 0")?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault_index {} must fit u8 for Squads account index",
            vault.vault_index
        )
    })?;
    if vault.threshold != 1 {
        return Err(format!(
            "policy update script only supports threshold 1, got {}",
            vault.threshold
        )
        .into());
    }

    let authority_signer = solana_testing_keypair_from_env()?;
    if authority_signer.pubkey() != authority {
        return Err(format!(
            "SOLANA_TESTING_PK pubkey {} does not match policy authority {}",
            authority_signer.pubkey(),
            authority
        )
        .into());
    }
    let policy_lookup_table_scope = format!(
        "same_mint_policy:{}:{}:{}",
        vault.settings, vault.vault_index, vault.policy_account
    );
    let lookup_table_pubkeys = lookup_table_pubkeys_for_scope(
        client,
        options,
        &policy_lookup_table_scope,
        authority_signer.pubkey(),
    )
    .await?;
    let mut lookup_table_accounts =
        load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let delegated_signer = policy_keypair_from_env()?;
    let db_delegated_signer_matches = vault
        .delegated_signers
        .iter()
        .any(|signer| signer == &delegated_signer.pubkey().to_string());

    let final_universe = same_mint_usdc_policy_universe()?;
    let swap_lanes = Vec::new();
    let context = LoyalActionContext {
        settings,
        authority,
        delegated_signer: delegated_signer.pubkey(),
        account_index,
        vault: vault_pubkey,
    };

    let existing_policy_account =
        rpc.get_account_with_commitment(&policy, CommitmentConfig::confirmed())?;
    let policy_exists = if let Some(account) = existing_policy_account.value.as_ref() {
        if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
            return Err(format!(
                "policy account {} is owned by {}, expected {}",
                policy, account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID
            )
            .into());
        }
        true
    } else {
        false
    };

    let all_in_one_setup = if policy_exists {
        update_all_in_one_market_mint_yield_route_action(
            context,
            final_universe.clone(),
            swap_lanes.clone(),
            policy,
            account_index,
        )?
    } else {
        let setup = YieldRouteActionBuilder::new(context, final_universe.clone())
            .topology(RouteTopology::AllInOne)
            .swap_lanes(swap_lanes.clone())
            .seeds(YieldRouteActionSeeds {
                withdraw: policy_seed,
                ..YieldRouteActionSeeds::default()
            })
            .build()?;
        if setup.accounts.withdraw != policy {
            return Err(format!(
                "policy seed {} derives {}, but DB policy_account is {}",
                policy_seed, setup.accounts.withdraw, policy
            )
            .into());
        }
        setup
    };
    let existing_decoded = existing_policy_account
        .value
        .as_ref()
        .and_then(|account| decode_squads_policy_account(&account.data).ok());
    let all_in_one_instruction = all_in_one_setup
        .instructions
        .first()
        .ok_or(if policy_exists {
            "policy update did not produce an instruction"
        } else {
            "policy create did not produce an instruction"
        })?
        .clone();
    let all_in_one_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        all_in_one_instruction.clone(),
        &lookup_table_accounts,
        &authority_signer,
        if policy_exists {
            "policy all-in-one update measurement"
        } else {
            "policy all-in-one create measurement"
        },
        None,
    )?;
    let all_in_one_preview = policy_operation_preview_json(
        if policy_exists {
            "all_in_one_update_attempt"
        } else {
            "all_in_one_create_attempt"
        },
        vault,
        settings,
        policy,
        vault_pubkey,
        authority_signer.pubkey(),
        delegated_signer.pubkey(),
        db_delegated_signer_matches,
        &final_universe,
        &swap_lanes,
        &all_in_one_setup,
        &all_in_one_transaction,
        existing_decoded.as_ref(),
    )?;
    let all_in_one_best_case_fits = all_in_one_transaction
        .best_case_single_lookup_table_packet
        .as_ref()
        .map(|packet| packet.fits_packet_data_size)
        .unwrap_or(
            all_in_one_transaction
                .transaction_packet
                .fits_packet_data_size,
        );

    if all_in_one_best_case_fits {
        let required_lookup_addresses = best_case_lookup_table_addresses(
            authority_signer.pubkey(),
            &[all_in_one_instruction.clone()],
            &[authority_signer.pubkey()],
        );
        let lookup_table_provisioning = prepare_durable_route_lookup_table(
            client,
            &rpc,
            options,
            &policy_lookup_table_scope,
            authority_signer.pubkey(),
            &authority_signer,
            &required_lookup_addresses,
            &mut lookup_table_accounts,
        )
        .await?;
        let policy_transaction = build_policy_transaction(
            &rpc,
            authority_signer.pubkey(),
            all_in_one_instruction,
            &lookup_table_accounts,
            &authority_signer,
            if policy_exists {
                "policy update"
            } else {
                "policy create"
            },
            None,
        )?;
        let policy_preview = policy_operation_preview_json(
            if policy_exists { "update" } else { "create" },
            vault,
            settings,
            policy,
            vault_pubkey,
            authority_signer.pubkey(),
            delegated_signer.pubkey(),
            db_delegated_signer_matches,
            &final_universe,
            &swap_lanes,
            &all_in_one_setup,
            &policy_transaction,
            existing_decoded.as_ref(),
        )?;

        if !options.execute {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "policy_update_dry_run",
                    "writesDecision": false,
                    "sendsTransactions": false,
                    "fallbackRequired": false,
                    "lookupTableProvisioning": lookup_table_provisioning.clone(),
                    "policyAllInOneAttempt": all_in_one_preview,
                    "policyCreate": if policy_exists { None } else { Some(policy_preview.clone()) },
                    "policyUpdate": if policy_exists { Some(policy_preview.clone()) } else { None },
                    "policyFinalizeUpdate": Value::Null,
                }))?
            );
            return Ok(());
        }

        if let Some(error) = policy_transaction.simulation_error.clone() {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "policy_update_simulation_failed",
                    "writesDecision": false,
                    "sendsTransactions": false,
                    "fallbackRequired": false,
                    "lookupTableProvisioning": lookup_table_provisioning.clone(),
                    "policyAllInOneAttempt": all_in_one_preview,
                    "policyCreate": if policy_exists { None } else { Some(policy_preview.clone()) },
                    "policyUpdate": if policy_exists { Some(policy_preview.clone()) } else { None },
                    "policyFinalizeUpdate": Value::Null,
                }))?
            );
            return Err(format!(
                "policy {} simulation failed: {error}",
                if policy_exists { "update" } else { "create" }
            )
            .into());
        }

        let submitted_slot = rpc.get_slot()?;
        let signature = rpc
            .send_and_confirm_transaction(&policy_transaction.transaction)?
            .to_string();
        let confirmed_slot = rpc.get_slot()?;
        let create_signature = if policy_exists {
            None
        } else {
            Some(signature.clone())
        };
        let create_submitted_slot = if policy_exists {
            None
        } else {
            Some(i64::try_from(submitted_slot)?)
        };
        let create_confirmed_slot = if policy_exists {
            None
        } else {
            Some(i64::try_from(confirmed_slot)?)
        };
        let policy_swap_lanes = policy_swap_lanes_json(&all_in_one_setup, &swap_lanes)?;
        let stored = client
            .record_policy_match(PolicyMatchInput {
                signature: signature.clone(),
                slot: confirmed_slot,
                settings: settings.to_string(),
                authority: authority.to_string(),
                policy_seed,
                policy_account: policy.to_string(),
                vault_index: account_index,
                vault_pubkey: vault_pubkey.to_string(),
                delegated_signers: vec![delegated_signer.pubkey().to_string()],
                threshold: 1,
                route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
                stable_mints: pubkeys_json(&final_universe.stable_mints),
                kamino_markets: pubkeys_json(&final_universe.kamino_markets),
                kamino_liquidity_mints: pubkeys_json(&final_universe.kamino_liquidity_mints),
                universe_preset: Some(KAMINO_STABLE_UNIVERSE_PRESET.to_owned()),
                risk_profile: Some(SAFE_RISK_PROFILE.to_owned()),
                swap_lanes: policy_swap_lanes.clone(),
            })
            .await?;
        let updated_account = rpc.get_account(&policy)?;
        let updated_decoded =
            decode_squads_policy_account(&updated_account.data).map_err(|error| {
                format!("failed to decode updated Squads policy account {policy}: {error}")
            })?;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": if policy_exists { "policy_updated" } else { "policy_created" },
                "writesDecision": false,
                "sendsTransactions": true,
                "fallbackRequired": false,
                "lookupTableProvisioning": lookup_table_provisioning,
                "signature": signature,
                "submittedSlot": i64::try_from(submitted_slot)?,
                "confirmedSlot": i64::try_from(confirmed_slot)?,
                "createSignature": create_signature,
                "createSubmittedSlot": create_submitted_slot,
                "createConfirmedSlot": create_confirmed_slot,
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
                "storedPolicyMatch": {
                    "policyId": stored.policy.id.as_i64(),
                    "vaultId": stored.vault.id.as_i64(),
                    "vaultActive": stored.vault.active,
                    "activePolicyId": stored.vault.active_policy_id.as_i64(),
                    "setupPolicyId": Value::Null,
                    "policyActive": stored.policy.active,
                },
                "updatedPolicyDecoded": decoded_policy_account_json(&updated_decoded),
                "decodedAllowsInitObligation": updated_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)),
                "decodedAllowsRefreshObligation": updated_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP)),
            }))?
        );
        return Ok(());
    }

    let setup_policy_seed = vault
        .setup_policy_seed
        .unwrap_or_else(|| vault.policy_seed.saturating_add(1));
    let setup_policy_seed_u64 =
        u64::try_from(setup_policy_seed).map_err(|_| "setup_policy_seed must be >= 0")?;
    let setup_policy = vault
        .setup_policy_account
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?
        .unwrap_or_else(|| derive_action_account(&settings, setup_policy_seed_u64).0);
    let route_setup = if policy_exists {
        update_same_mint_market_mint_yield_route_action(
            context,
            final_universe.clone(),
            policy,
            account_index,
        )?
    } else {
        let setup = create_same_mint_market_mint_yield_route_action(
            context,
            final_universe.clone(),
            policy_seed,
        )?;
        if setup.accounts.withdraw != policy {
            return Err(format!(
                "route policy seed {} derives {}, but DB policy_account is {}",
                policy_seed, setup.accounts.withdraw, policy
            )
            .into());
        }
        setup
    };
    let setup_existing_account =
        rpc.get_account_with_commitment(&setup_policy, CommitmentConfig::confirmed())?;
    let setup_policy_exists = if let Some(account) = setup_existing_account.value.as_ref() {
        if account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
            return Err(format!(
                "setup policy account {} is owned by {}, expected {}",
                setup_policy, account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID
            )
            .into());
        }
        true
    } else {
        false
    };
    let setup_existing_decoded = setup_existing_account
        .value
        .as_ref()
        .and_then(|account| decode_squads_policy_account(&account.data).ok());
    let setup_policy_setup = if setup_policy_exists {
        update_init_obligation_yield_route_action(
            context,
            final_universe.clone(),
            setup_policy,
            account_index,
        )?
    } else {
        let setup = create_init_obligation_yield_route_action(
            context,
            final_universe.clone(),
            setup_policy_seed_u64,
        )?;
        if setup.accounts.withdraw != setup_policy {
            return Err(format!(
                "setup policy seed {} derives {}, but expected setup policy {}",
                setup_policy_seed, setup.accounts.withdraw, setup_policy
            )
            .into());
        }
        setup
    };
    let route_instruction = route_setup
        .instructions
        .first()
        .ok_or("fallback route policy instruction was not built")?
        .clone();
    let setup_instruction = setup_policy_setup
        .instructions
        .first()
        .ok_or("fallback setup policy instruction was not built")?
        .clone();
    let required_lookup_addresses = best_case_lookup_table_addresses(
        authority_signer.pubkey(),
        &[route_instruction.clone(), setup_instruction.clone()],
        &[authority_signer.pubkey()],
    );
    let lookup_table_provisioning = prepare_durable_route_lookup_table(
        client,
        &rpc,
        options,
        &policy_lookup_table_scope,
        authority_signer.pubkey(),
        &authority_signer,
        &required_lookup_addresses,
        &mut lookup_table_accounts,
    )
    .await?;
    let setup_policy_requires_landed_route_create = !policy_exists && !setup_policy_exists;
    let setup_policy_simulation_skip_reason = setup_policy_requires_landed_route_create.then(|| {
        "setup policy create uses the next Squads policy seed and must be simulated after the route policy create lands".to_owned()
    });
    let route_policy_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        route_instruction,
        &lookup_table_accounts,
        &authority_signer,
        if policy_exists {
            "route policy fallback update"
        } else {
            "route policy fallback create"
        },
        None,
    )?;
    let mut setup_policy_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        setup_instruction.clone(),
        &lookup_table_accounts,
        &authority_signer,
        if setup_policy_exists {
            "setup policy fallback update"
        } else {
            "setup policy fallback create"
        },
        setup_policy_simulation_skip_reason,
    )?;
    let route_policy_preview = policy_operation_preview_json(
        if policy_exists { "update" } else { "create" },
        vault,
        settings,
        policy,
        vault_pubkey,
        authority_signer.pubkey(),
        delegated_signer.pubkey(),
        db_delegated_signer_matches,
        &final_universe,
        &swap_lanes,
        &route_setup,
        &route_policy_transaction,
        existing_decoded.as_ref(),
    )?;
    let mut setup_policy_preview = setup_policy_operation_preview_json(
        if setup_policy_exists {
            "update"
        } else {
            "create"
        },
        vault,
        settings,
        setup_policy,
        setup_policy_seed,
        vault_pubkey,
        authority_signer.pubkey(),
        delegated_signer.pubkey(),
        db_delegated_signer_matches,
        &final_universe,
        &setup_policy_setup,
        &setup_policy_transaction,
        setup_existing_decoded.as_ref(),
    )?;

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "policy_update_dry_run",
                "writesDecision": false,
                "sendsTransactions": false,
                "fallbackRequired": true,
                "fallbackReason": "all_safe_one_policy_exceeds_packet_limit",
                "lookupTableProvisioning": lookup_table_provisioning.clone(),
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
                "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
                "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
            }))?
        );
        return Ok(());
    }

    if let Some(error) = route_policy_transaction.simulation_error.clone() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "policy_update_simulation_failed",
                "writesDecision": false,
                "sendsTransactions": false,
                "fallbackRequired": true,
                "lookupTableProvisioning": lookup_table_provisioning.clone(),
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
                "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
                "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
            }))?
        );
        return Err(format!("fallback route policy simulation failed: {error}").into());
    }
    if let Some(error) = setup_policy_transaction.simulation_error.clone() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "policy_update_simulation_failed",
                "writesDecision": false,
                "sendsTransactions": false,
                "fallbackRequired": true,
                "lookupTableProvisioning": lookup_table_provisioning.clone(),
                "policyAllInOneAttempt": all_in_one_preview,
                "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
                "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
                "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
                "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
                "policyFinalizeUpdate": Value::Null,
            }))?
        );
        return Err(format!("fallback setup policy simulation failed: {error}").into());
    }

    let route_submitted_slot = rpc.get_slot()?;
    let route_signature = rpc
        .send_and_confirm_transaction(&route_policy_transaction.transaction)?
        .to_string();
    let route_confirmed_slot = rpc.get_slot()?;
    if setup_policy_requires_landed_route_create {
        setup_policy_transaction = build_policy_transaction(
            &rpc,
            authority_signer.pubkey(),
            setup_instruction,
            &lookup_table_accounts,
            &authority_signer,
            "setup policy fallback create",
            None,
        )?;
        setup_policy_preview = setup_policy_operation_preview_json(
            "create",
            vault,
            settings,
            setup_policy,
            setup_policy_seed,
            vault_pubkey,
            authority_signer.pubkey(),
            delegated_signer.pubkey(),
            db_delegated_signer_matches,
            &final_universe,
            &setup_policy_setup,
            &setup_policy_transaction,
            setup_existing_decoded.as_ref(),
        )?;
        if let Some(error) = setup_policy_transaction.simulation_error.clone() {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "policy_update_simulation_failed",
                    "writesDecision": false,
                    "sendsTransactions": true,
                    "fallbackRequired": true,
                    "lookupTableProvisioning": lookup_table_provisioning.clone(),
                    "routeSignature": route_signature,
                    "routeSubmittedSlot": i64::try_from(route_submitted_slot)?,
                    "routeConfirmedSlot": i64::try_from(route_confirmed_slot)?,
                    "policyAllInOneAttempt": all_in_one_preview,
                    "policyCreate": Some(route_policy_preview.clone()),
                    "policyUpdate": Value::Null,
                    "setupPolicyCreate": Some(setup_policy_preview.clone()),
                    "setupPolicyUpdate": Value::Null,
                    "policyFinalizeUpdate": Value::Null,
                }))?
            );
            return Err(format!(
                "fallback setup policy simulation failed after route policy create landed: {error}"
            )
            .into());
        }
    }
    let setup_submitted_slot = rpc.get_slot()?;
    let setup_signature = rpc
        .send_and_confirm_transaction(&setup_policy_transaction.transaction)?
        .to_string();
    let setup_confirmed_slot = rpc.get_slot()?;
    let create_signature = if policy_exists {
        None
    } else {
        Some(route_signature.clone())
    };
    let create_submitted_slot = if policy_exists {
        None
    } else {
        Some(i64::try_from(route_submitted_slot)?)
    };
    let create_confirmed_slot = if policy_exists {
        None
    } else {
        Some(i64::try_from(route_confirmed_slot)?)
    };
    let setup_create_signature = if setup_policy_exists {
        None
    } else {
        Some(setup_signature.clone())
    };
    let setup_create_submitted_slot = if setup_policy_exists {
        None
    } else {
        Some(i64::try_from(setup_submitted_slot)?)
    };
    let setup_create_confirmed_slot = if setup_policy_exists {
        None
    } else {
        Some(i64::try_from(setup_confirmed_slot)?)
    };
    let policy_swap_lanes = policy_swap_lanes_json(&route_setup, &swap_lanes)?;
    let (stored, stored_setup_policy) = client
        .record_route_and_setup_policy_match(
            PolicyMatchInput {
                signature: route_signature.clone(),
                slot: route_confirmed_slot,
                settings: settings.to_string(),
                authority: authority.to_string(),
                policy_seed,
                policy_account: policy.to_string(),
                vault_index: account_index,
                vault_pubkey: vault_pubkey.to_string(),
                delegated_signers: vec![delegated_signer.pubkey().to_string()],
                threshold: 1,
                route_modes: vec![SAME_MINT_ROUTE_MODE.to_owned()],
                stable_mints: pubkeys_json(&final_universe.stable_mints),
                kamino_markets: pubkeys_json(&final_universe.kamino_markets),
                kamino_liquidity_mints: pubkeys_json(&final_universe.kamino_liquidity_mints),
                universe_preset: Some(KAMINO_STABLE_UNIVERSE_PRESET.to_owned()),
                risk_profile: Some(SAFE_RISK_PROFILE.to_owned()),
                swap_lanes: policy_swap_lanes.clone(),
            },
            PolicyMatchInput {
                signature: setup_signature.clone(),
                slot: setup_confirmed_slot,
                settings: settings.to_string(),
                authority: authority.to_string(),
                policy_seed: setup_policy_seed_u64,
                policy_account: setup_policy.to_string(),
                vault_index: account_index,
                vault_pubkey: vault_pubkey.to_string(),
                delegated_signers: vec![delegated_signer.pubkey().to_string()],
                threshold: 1,
                route_modes: vec![format!("{SAME_MINT_ROUTE_MODE}_setup")],
                stable_mints: pubkeys_json(&final_universe.stable_mints),
                kamino_markets: pubkeys_json(&final_universe.kamino_markets),
                kamino_liquidity_mints: pubkeys_json(&final_universe.kamino_liquidity_mints),
                universe_preset: Some(KAMINO_STABLE_UNIVERSE_PRESET.to_owned()),
                risk_profile: Some(SAFE_RISK_PROFILE.to_owned()),
                swap_lanes: Value::Array(vec![]),
            },
        )
        .await?;
    let updated_route_account = rpc.get_account(&policy)?;
    let updated_route_decoded =
        decode_squads_policy_account(&updated_route_account.data).map_err(|error| {
            format!("failed to decode updated route policy account {policy}: {error}")
        })?;
    let updated_setup_account = rpc.get_account(&setup_policy)?;
    let updated_setup_decoded =
        decode_squads_policy_account(&updated_setup_account.data).map_err(|error| {
            format!("failed to decode updated setup policy account {setup_policy}: {error}")
        })?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": if policy_exists || setup_policy_exists { "policy_fallback_updated" } else { "policy_fallback_created" },
            "writesDecision": false,
            "sendsTransactions": true,
            "fallbackRequired": true,
            "fallbackReason": "all_safe_one_policy_exceeds_packet_limit",
            "lookupTableProvisioning": lookup_table_provisioning,
            "signature": route_signature,
            "submittedSlot": i64::try_from(route_submitted_slot)?,
            "confirmedSlot": i64::try_from(route_confirmed_slot)?,
            "createSignature": create_signature,
            "createSubmittedSlot": create_submitted_slot,
            "createConfirmedSlot": create_confirmed_slot,
            "setupSignature": setup_signature,
            "setupSubmittedSlot": i64::try_from(setup_submitted_slot)?,
            "setupConfirmedSlot": i64::try_from(setup_confirmed_slot)?,
            "setupCreateSignature": setup_create_signature,
            "setupCreateSubmittedSlot": setup_create_submitted_slot,
            "setupCreateConfirmedSlot": setup_create_confirmed_slot,
            "policyAllInOneAttempt": all_in_one_preview,
            "policyCreate": if policy_exists { None } else { Some(route_policy_preview.clone()) },
            "policyUpdate": if policy_exists { Some(route_policy_preview.clone()) } else { None },
            "setupPolicyCreate": if setup_policy_exists { None } else { Some(setup_policy_preview.clone()) },
            "setupPolicyUpdate": if setup_policy_exists { Some(setup_policy_preview.clone()) } else { None },
            "policyFinalizeUpdate": Value::Null,
            "storedPolicyMatch": {
                "policyId": stored.policy.id.as_i64(),
                "setupPolicyId": stored_setup_policy.id.as_i64(),
                "vaultId": stored.vault.id.as_i64(),
                "vaultActive": stored.vault.active,
                "activePolicyId": stored.vault.active_policy_id.as_i64(),
                "activePolicyRemainsRoutePolicy": stored.vault.active_policy_id == stored.policy.id,
                "policyActive": stored.policy.active,
                "setupPolicyActive": stored_setup_policy.active,
            },
            "updatedPolicyDecoded": decoded_policy_account_json(&updated_route_decoded),
            "updatedSetupPolicyDecoded": decoded_policy_account_json(&updated_setup_decoded),
            "decodedAllowsInitObligation": updated_setup_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)),
            "decodedRouteAllowsInitObligation": updated_route_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)),
            "decodedAllowsRefreshObligation": updated_route_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP))
                || updated_setup_decoded.instructions.iter().any(|instruction| instruction.route_step == Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP)),
        }))?
    );
    Ok(())
}

fn build_missing_obligation_setup_dry_run(
    options: &CliOptions,
    vault: &SelectedVault,
    target: &ChainPositionSummary,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<MissingObligationSetupDryRun, Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let lookup_table_pubkeys = lookup_table_pubkeys_from_options(options)?;
    let lookup_table_accounts = load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let delegated_signer = policy_keypair_from_env()?;
    let admin_fee_payer = if options.optimization_cycle {
        None
    } else {
        Some(solana_testing_keypair_from_env()?)
    };
    let fee_payer: &dyn Signer = admin_fee_payer
        .as_ref()
        .map(|keypair| keypair as &dyn Signer)
        .unwrap_or(&delegated_signer);
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault_index {} must fit u8 for Squads account index",
            vault.vault_index
        )
    })?;
    let (policy, instruction_constraint_index) =
        resolve_init_obligation_policy(Some(&rpc), vault, target, policy_preflight)?;
    let route_policy = Pubkey::from_str(&vault.policy_account)?;
    let policy_source = if policy == route_policy {
        "route_policy"
    } else {
        "setup_policy"
    };

    let init_execution = build_init_obligation_execution_transaction(
        &rpc,
        &lookup_table_accounts,
        policy,
        account_index,
        vault_pubkey,
        target,
        instruction_constraint_index,
        fee_payer,
        &delegated_signer,
        None,
    )?;

    Ok(MissingObligationSetupDryRun {
        policy_account: policy.to_string(),
        policy_source,
        instruction_constraint_index,
        init_execution,
    })
}

async fn execute_missing_obligation_setup(
    options: &CliOptions,
    vault: &SelectedVault,
    target: &ChainPositionSummary,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<MissingObligationSetupSubmitResult, Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let lookup_table_pubkeys = lookup_table_pubkeys_from_options(options)?;
    let lookup_table_accounts = load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let delegated_signer = policy_keypair_from_env()?;
    let admin_fee_payer = if options.optimization_cycle {
        None
    } else {
        Some(solana_testing_keypair_from_env()?)
    };
    let fee_payer: &dyn Signer = admin_fee_payer
        .as_ref()
        .map(|keypair| keypair as &dyn Signer)
        .unwrap_or(&delegated_signer);
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault_index {} must fit u8 for Squads account index",
            vault.vault_index
        )
    })?;
    let (policy, instruction_constraint_index) =
        resolve_init_obligation_policy(Some(&rpc), vault, target, policy_preflight)?;
    let route_policy = Pubkey::from_str(&vault.policy_account)?;
    let policy_source = if policy == route_policy {
        "route_policy"
    } else {
        "setup_policy"
    };
    let init_execution = build_init_obligation_execution_transaction(
        &rpc,
        &lookup_table_accounts,
        policy,
        account_index,
        vault_pubkey,
        target,
        instruction_constraint_index,
        fee_payer,
        &delegated_signer,
        None,
    )?;
    if let Some(error) = &init_execution.simulation_error {
        return Err(format!("init-obligation execution simulation failed: {error}").into());
    }
    let init_submission = submit_built_policy_transaction(&rpc, &init_execution)?;

    Ok(MissingObligationSetupSubmitResult {
        policy_account: policy.to_string(),
        policy_source,
        instruction_constraint_index,
        init_signature: init_submission.signature,
        init_submitted_slot: init_submission.submitted_slot,
        init_confirmed_slot: init_submission.confirmed_slot,
        init_simulation_units_consumed: init_execution.simulation_units_consumed,
        init_transaction_packet: init_execution.transaction_packet,
    })
}

async fn run_setup_obligation_flow(
    options: &CliOptions,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    setup_reserve: &str,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<(), Box<dyn Error>> {
    let target = chain_position_for_reserve(preview, setup_reserve)?;
    if target.obligation_exists {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "setup_obligation_reserve_skipped_existing",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "execute": options.execute,
                "vault": vault_json(vault),
                "target": {
                    "reserve": target.reserve,
                    "market": target.market,
                    "liquidityMint": target.liquidity_mint,
                    "obligation": target.obligation,
                    "obligationExists": true,
                },
                "chainReconcile": chain_reconcile_preview_json(preview),
            }))?
        );
        return Ok(());
    }

    if !options.execute {
        let dry_run =
            build_missing_obligation_setup_dry_run(options, vault, target, policy_preflight)?;
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "setup_obligation_reserve_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "execute": false,
                "vault": vault_json(vault),
                "target": {
                    "reserve": target.reserve,
                    "market": target.market,
                    "liquidityMint": target.liquidity_mint,
                    "obligation": target.obligation,
                    "obligationExists": false,
                },
                "chainReconcile": chain_reconcile_preview_json(preview),
                "missingObligationSetup": missing_obligation_setup_dry_run_json(target, &dry_run),
            }))?
        );
        return Ok(());
    }

    let result = execute_missing_obligation_setup(options, vault, target, policy_preflight).await?;
    let post_preview = load_chain_reconcile_preview(
        &options.rpc_url,
        vault,
        &preview
            .positions
            .iter()
            .map(|position| position.reserve.clone())
            .collect::<Vec<_>>(),
    )?;
    let post_target = chain_position_for_reserve(&post_preview, setup_reserve)?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "setup_obligation_reserve_executed",
            "writesDecision": false,
            "writesCurrentPositions": false,
            "sendsTransactions": true,
            "execute": true,
            "vault": vault_json(vault),
            "target": {
                "reserve": post_target.reserve,
                "market": post_target.market,
                "liquidityMint": post_target.liquidity_mint,
                "obligation": post_target.obligation,
                "obligationExists": post_target.obligation_exists,
            },
            "setup": missing_obligation_setup_submit_result_json(target, &result),
            "postChainReconcile": chain_reconcile_preview_json(&post_preview),
        }))?
    );
    Ok(())
}

fn submit_built_policy_transaction(
    rpc: &RpcClient,
    transaction: &PolicyTransactionBuild,
) -> Result<SubmittedPolicyTransaction, Box<dyn Error>> {
    let submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = rpc
        .send_and_confirm_transaction(&transaction.transaction)?
        .to_string();
    let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    Ok(SubmittedPolicyTransaction {
        signature,
        submitted_slot,
        confirmed_slot,
    })
}

fn build_init_obligation_execution_transaction(
    rpc: &RpcClient,
    lookup_table_accounts: &[AddressLookupTableAccount],
    policy: Pubkey,
    account_index: u8,
    vault_pubkey: Pubkey,
    target: &ChainPositionSummary,
    instruction_constraint_index: u8,
    fee_payer: &dyn Signer,
    delegated_signer: &dyn Signer,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    let init_instruction = kamino_init_obligation_instruction(vault_pubkey, target)?;
    let mut transaction_accounts = Vec::new();
    let init_compiled =
        compile_squads_inner_instruction(&mut transaction_accounts, init_instruction);
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy,
        delegated_signer.pubkey(),
        account_index,
        vec![init_compiled],
        vec![instruction_constraint_index],
        transaction_accounts,
    );
    let transaction_signers = same_mint_route_signers(fee_payer, delegated_signer);
    build_signed_transaction(
        rpc,
        fee_payer.pubkey(),
        &[outer_instruction],
        lookup_table_accounts,
        &transaction_signers,
        "init-obligation setup execution",
        simulation_skip_reason,
    )
}

async fn run_initial_reserve_deposit_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    initial_preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    deposit_reserve: &str,
    amount_raw: u64,
) -> Result<(), Box<dyn Error>> {
    if amount_raw == 0 {
        return Err("initial deposit amount must be greater than 0".into());
    }

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let lookup_table_pubkeys = lookup_table_pubkeys_from_options(options)?;
    let lookup_table_accounts = load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let wallet_signer = solana_testing_keypair_from_env()?;
    let delegated_signer = policy_keypair_from_env()?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let deposit_position = chain_position_for_reserve(initial_preview, deposit_reserve)?;
    let mut active_preview = initial_preview.clone();
    let mut reloaded_policy_preflight: Option<PolicyAccountPreflight> = None;
    let mut missing_obligation_setup_result: Option<Value> = None;
    let missing_obligation_setup_dry_run =
        if !options.execute && !deposit_position.obligation_exists {
            Some(
                build_missing_obligation_setup_dry_run(
                    options,
                    vault,
                    deposit_position,
                    policy_preflight,
                )
                .map(|dry_run| missing_obligation_setup_dry_run_json(deposit_position, &dry_run))
                .unwrap_or_else(|error| {
                    json!({
                        "targetObligation": deposit_position.obligation,
                        "targetReserve": deposit_position.reserve,
                        "targetMarket": deposit_position.market,
                        "error": error.to_string(),
                    })
                }),
            )
        } else {
            None
        };
    let wallet_usdc_ata =
        derive_associated_token_address(&wallet_signer.pubkey(), &USDC_MINT, &spl_token::ID);
    let vault_usdc_ata = derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let (wallet_usdc_amount_raw, wallet_usdc_account_exists) =
        load_spl_token_account_amount(&rpc, &wallet_usdc_ata, &USDC_MINT)?;
    let funding_needed_raw = amount_raw.saturating_sub(deposit_position.vault_liquidity_amount_raw);
    let mut blockers = Vec::new();
    if !wallet_usdc_account_exists {
        blockers.push(format!(
            "wallet USDC ATA {} does not exist for {}",
            wallet_usdc_ata,
            wallet_signer.pubkey()
        ));
    }
    if wallet_usdc_amount_raw < funding_needed_raw {
        blockers.push(format!(
            "wallet USDC balance {} is below needed funding amount {}",
            wallet_usdc_amount_raw, funding_needed_raw
        ));
    }
    if !deposit_position.obligation_exists && !options.execute {
        blockers.push(format!(
            "deposit obligation {} is missing for reserve {}; run missing-obligation setup before policy deposit",
            deposit_position.obligation, deposit_position.reserve
        ));
    }
    if options.execute && blockers.iter().any(|reason| reason.contains("wallet USDC")) {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "initial_deposit_preflight_blocked",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "preflightBlockers": blockers,
                "missingObligationSetup": Value::Null,
            }))?
        );
        return Err("initial reserve deposit preflight blocked before setup".into());
    }
    if options.execute && !deposit_position.obligation_exists {
        let setup_result =
            execute_missing_obligation_setup(options, vault, deposit_position, policy_preflight)
                .await?;
        missing_obligation_setup_result = Some(missing_obligation_setup_submit_result_json(
            deposit_position,
            &setup_result,
        ));
        active_preview =
            load_chain_reconcile_preview(&options.rpc_url, vault, &[deposit_reserve.to_owned()])?;
        reloaded_policy_preflight = Some(load_policy_account_preflight(
            &options.rpc_url,
            vault,
            &active_preview,
            &ReserveMove {
                source_reserve: deposit_reserve.to_owned(),
                target_reserve: deposit_reserve.to_owned(),
            },
        )?);
        let active_deposit = chain_position_for_reserve(&active_preview, deposit_reserve)?;
        if !active_deposit.obligation_exists {
            return Err(format!(
                "deposit obligation {} is still missing after setup execution",
                active_deposit.obligation
            )
            .into());
        }
    }
    let active_policy_preflight = reloaded_policy_preflight.as_ref().or(policy_preflight);

    let mut funding_instructions = vec![create_associated_token_account_idempotent_instruction(
        wallet_signer.pubkey(),
        vault_pubkey,
        USDC_MINT,
        spl_token::ID,
    )];
    if funding_needed_raw > 0 {
        funding_instructions.push(spl_token::instruction::transfer_checked(
            &spl_token::ID,
            &wallet_usdc_ata,
            &USDC_MINT,
            &vault_usdc_ata,
            &wallet_signer.pubkey(),
            &[],
            funding_needed_raw,
            6,
        )?);
    }
    let funding_skip_reason = if blockers.iter().any(|reason| reason.contains("wallet USDC")) {
        Some("funding simulation skipped because wallet USDC preflight failed".to_owned())
    } else {
        None
    };
    let funding_transaction = build_signed_transaction(
        &rpc,
        wallet_signer.pubkey(),
        &funding_instructions,
        &lookup_table_accounts,
        &[&wallet_signer],
        "initial reserve funding",
        funding_skip_reason,
    )?;

    let policy_plan = match build_initial_reserve_deposit_policy_plan(
        vault,
        &active_preview,
        active_policy_preflight,
        deposit_reserve,
        amount_raw,
        wallet_signer.pubkey(),
        delegated_signer.pubkey(),
        account_index,
    ) {
        Ok(plan) => Some(plan),
        Err(error) => {
            blockers.push(error.to_string());
            None
        }
    };
    let dry_run_policy_transaction = if let Some(plan) = policy_plan.as_ref() {
        let policy_simulation_skip_reason =
            if deposit_position.vault_liquidity_amount_raw >= amount_raw {
                None
            } else {
                Some(
                "policy deposit simulation requires the wallet funding transaction to land first"
                    .to_owned(),
            )
            };
        let mut policy_instructions = plan.pre_instructions.clone();
        policy_instructions.push(plan.instruction.clone());
        Some(build_signed_transaction(
            &rpc,
            wallet_signer.pubkey(),
            &policy_instructions,
            &lookup_table_accounts,
            &[&wallet_signer, &delegated_signer],
            "initial reserve policy deposit",
            policy_simulation_skip_reason,
        )?)
    } else {
        None
    };

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "initial_deposit_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": {
                    "reserve": &deposit_position.reserve,
                    "market": &deposit_position.market,
                    "liquidityMint": USDC_MINT.to_string(),
                    "amountRaw": amount_raw.to_string(),
                },
                "wallet": {
                    "signer": wallet_signer.pubkey().to_string(),
                    "usdcAta": wallet_usdc_ata.to_string(),
                    "usdcAtaExists": wallet_usdc_account_exists,
                    "usdcAmountRaw": wallet_usdc_amount_raw.to_string(),
                },
                "vault": vault_json(vault),
                "vaultUsdcAta": vault_usdc_ata.to_string(),
                "chainReconcile": chain_reconcile_preview_json(initial_preview),
                "activeChainReconcile": chain_reconcile_preview_json(&active_preview),
                "policyPreflight": policy_route_preflight_json(vault, &ReserveMove {
                    source_reserve: deposit_reserve.to_owned(),
                    target_reserve: deposit_reserve.to_owned(),
                }, active_policy_preflight),
                "preflightBlockers": blockers,
                "missingObligationSetup": missing_obligation_setup_dry_run,
                "fundingTransaction": policy_transaction_json(&funding_transaction),
                "policyDeposit": policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": dry_run_policy_transaction.as_ref().map(policy_transaction_json),
            }))?
        );
        return Ok(());
    }

    if !blockers.is_empty() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "initial_deposit_preflight_blocked",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "preflightBlockers": blockers,
                "missingObligationSetup": missing_obligation_setup_result.clone(),
                "fundingTransaction": policy_transaction_json(&funding_transaction),
                "policyDeposit": policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
            }))?
        );
        return Err("initial reserve deposit preflight blocked before live submit".into());
    }
    if let Some(error) = &funding_transaction.simulation_error {
        return Err(format!("initial reserve funding simulation failed: {error}").into());
    }

    let funding_submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let funding_signature = rpc.send_and_confirm_transaction(&funding_transaction.transaction)?;
    let funding_confirmed_slot = i64::try_from(rpc.get_slot()?)?;

    let funded_preview =
        load_chain_reconcile_preview(&options.rpc_url, vault, &[deposit_reserve.to_owned()])?;
    let funded_deposit_position = chain_position_for_reserve(&funded_preview, deposit_reserve)?;
    if funded_deposit_position.vault_liquidity_amount_raw < amount_raw {
        return Err(format!(
            "vault USDC ATA {} has {} after funding, below requested deposit {}",
            funded_deposit_position.vault_liquidity_ata,
            funded_deposit_position.vault_liquidity_amount_raw,
            amount_raw
        )
        .into());
    }

    let policy_plan = build_initial_reserve_deposit_policy_plan(
        vault,
        &funded_preview,
        active_policy_preflight,
        deposit_reserve,
        amount_raw,
        wallet_signer.pubkey(),
        delegated_signer.pubkey(),
        account_index,
    )?;
    let mut policy_instructions = policy_plan.pre_instructions.clone();
    policy_instructions.push(policy_plan.instruction.clone());
    let policy_transaction = build_signed_transaction(
        &rpc,
        wallet_signer.pubkey(),
        &policy_instructions,
        &lookup_table_accounts,
        &[&wallet_signer, &delegated_signer],
        "initial reserve policy deposit",
        None,
    )?;
    if let Some(error) = &policy_transaction.simulation_error {
        return Err(format!(
            "initial reserve policy deposit simulation failed after funding tx {}: {error}",
            funding_signature
        )
        .into());
    }

    let policy_submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let policy_signature = rpc.send_and_confirm_transaction(&policy_transaction.transaction)?;
    let policy_confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    let post_preview =
        load_chain_reconcile_preview(&options.rpc_url, vault, &[deposit_reserve.to_owned()])?;
    let snapshot = client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(&post_preview)?)
        .await?;
    let result = InitialDepositSubmitResult {
        funding_signature: Some(funding_signature.to_string()),
        funding_submitted_slot: Some(funding_submitted_slot),
        funding_confirmed_slot: Some(funding_confirmed_slot),
        funding_simulation_units_consumed: funding_transaction.simulation_units_consumed,
        funding_transaction_packet: funding_transaction.transaction_packet,
        policy_signature: Some(policy_signature.to_string()),
        policy_submitted_slot: Some(policy_submitted_slot),
        policy_confirmed_slot: Some(policy_confirmed_slot),
        policy_simulation_units_consumed: policy_transaction.simulation_units_consumed,
        policy_transaction_packet: policy_transaction.transaction_packet,
        reconciled_snapshot_id: Some(snapshot.id),
        post_chain_preview: Some(post_preview),
    };

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "initial_deposit_executed",
            "writesDecision": false,
            "writesCurrentPositions": true,
            "sendsTransactions": true,
            "deposit": {
                "reserve": deposit_reserve,
                "market": &funded_deposit_position.market,
                "liquidityMint": USDC_MINT.to_string(),
                "amountRaw": amount_raw.to_string(),
            },
            "wallet": {
                "signer": wallet_signer.pubkey().to_string(),
                "usdcAta": wallet_usdc_ata.to_string(),
            },
            "vault": vault_json(vault),
            "vaultUsdcAta": vault_usdc_ata.to_string(),
            "missingObligationSetup": missing_obligation_setup_result,
            "fundingTransaction": {
                "signature": result.funding_signature,
                "submittedSlot": result.funding_submitted_slot,
                "confirmedSlot": result.funding_confirmed_slot,
                "simulationUnitsConsumed": result.funding_simulation_units_consumed,
                "transaction": transaction_packet_json(&result.funding_transaction_packet),
            },
            "policyDeposit": initial_deposit_policy_preview_json(&policy_plan.preview),
            "policyDepositTransaction": {
                "signature": result.policy_signature,
                "submittedSlot": result.policy_submitted_slot,
                "confirmedSlot": result.policy_confirmed_slot,
                "simulationUnitsConsumed": result.policy_simulation_units_consumed,
                "transaction": transaction_packet_json(&result.policy_transaction_packet),
            },
            "reconciledSnapshotId": result.reconciled_snapshot_id.map(SnapshotId::as_i64),
            "postChainReconcile": result.post_chain_preview.as_ref().map(chain_reconcile_preview_json),
        }))?
    );

    Ok(())
}

async fn run_idle_vault_deposit_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    initial_preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    deposit_reserve: &str,
    amount_raw: u64,
) -> Result<(), Box<dyn Error>> {
    if amount_raw == 0 {
        return Err("idle vault deposit amount must be greater than 0".into());
    }

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let lookup_table_pubkeys = lookup_table_pubkeys_from_options(options)?;
    let lookup_table_accounts = load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let signer = policy_keypair_from_env()?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let deposit_position = chain_position_for_reserve(initial_preview, deposit_reserve)?;
    let vault_usdc_ata = derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let db_idle = client
        .current_idle_token_balance(vault.id, &USDC_MINT.to_string())
        .await?;
    let amount_i64 = i64::try_from(amount_raw)
        .map_err(|_| "idle vault deposit amount does not fit Postgres BIGINT")?;

    let mut blockers = Vec::new();
    if deposit_position.liquidity_mint != USDC_MINT.to_string() {
        blockers.push(format!(
            "target reserve {} liquidity mint {} is not USDC {}",
            deposit_position.reserve, deposit_position.liquidity_mint, USDC_MINT
        ));
    }
    if deposit_position.vault_liquidity_ata != vault_usdc_ata.to_string() {
        blockers.push(format!(
            "chain preview vault liquidity ATA {} does not match derived vault USDC ATA {}",
            deposit_position.vault_liquidity_ata, vault_usdc_ata
        ));
    }
    if !deposit_position.vault_liquidity_token_account_exists {
        blockers.push(format!(
            "vault idle USDC ATA {} does not exist",
            vault_usdc_ata
        ));
    }
    if deposit_position.vault_liquidity_amount_raw < amount_raw {
        blockers.push(format!(
            "live vault idle USDC balance {} is below planned deposit amount {}",
            deposit_position.vault_liquidity_amount_raw, amount_raw
        ));
    }
    if !deposit_position.obligation_exists {
        blockers.push(format!(
            "deposit obligation {} is missing for reserve {}; idle vault deposits require existing obligation setup",
            deposit_position.obligation, deposit_position.reserve
        ));
    }

    match db_idle.as_ref() {
        Some(balance) => {
            if balance.mint != USDC_MINT.to_string() {
                blockers.push(format!(
                    "DB idle mint {} does not match USDC {}",
                    balance.mint, USDC_MINT
                ));
            }
            if balance.token_account != vault_usdc_ata.to_string() {
                blockers.push(format!(
                    "DB idle token account {} does not match vault USDC ATA {}",
                    balance.token_account, vault_usdc_ata
                ));
            }
            if balance.amount_raw != amount_i64 {
                blockers.push(format!(
                    "DB idle amount {} does not match planned amount {}",
                    balance.amount_raw, amount_i64
                ));
            }
            if balance.amount_raw > i64::try_from(deposit_position.vault_liquidity_amount_raw)? {
                blockers.push(format!(
                    "DB idle amount {} is above live vault ATA balance {}",
                    balance.amount_raw, deposit_position.vault_liquidity_amount_raw
                ));
            }
            if let Some(expected_account) = &options.expected_idle_token_account {
                if balance.token_account != *expected_account {
                    blockers.push(format!(
                        "expected idle token account {} does not match DB row {}",
                        expected_account, balance.token_account
                    ));
                }
            }
            if let Some(expected_slot) = options.expected_idle_observed_slot {
                if balance.observed_slot != expected_slot {
                    blockers.push(format!(
                        "expected idle observed slot {} does not match DB row {}",
                        expected_slot, balance.observed_slot
                    ));
                }
            }
            if let Some(expected_at) = options.expected_idle_observed_at {
                if balance.observed_at != expected_at {
                    blockers.push(format!(
                        "expected idle observed at {} does not match DB row {}",
                        expected_at.to_rfc3339(),
                        balance.observed_at.to_rfc3339()
                    ));
                }
            }
        }
        None => blockers.push(format!(
            "missing loyal_yield.vault_idle_token_balances_current row for vault {} USDC",
            vault.id.as_i64()
        )),
    }

    if let Some(expected_account) = &options.expected_idle_token_account {
        if expected_account != &vault_usdc_ata.to_string() {
            blockers.push(format!(
                "expected idle token account {} does not match derived vault USDC ATA {}",
                expected_account, vault_usdc_ata
            ));
        }
    }
    if let Some(expected_mint) = &options.expected_liquidity_mint {
        if expected_mint != &USDC_MINT.to_string() {
            blockers.push(format!(
                "expected liquidity mint {} does not match USDC {}",
                expected_mint, USDC_MINT
            ));
        }
    }
    if let Some(expected_amount) = options.expected_amount_raw {
        if expected_amount != amount_i64 {
            blockers.push(format!(
                "expected amount {} does not match requested idle deposit amount {}",
                expected_amount, amount_i64
            ));
        }
    }
    if let Some(expected_edge) = options.expected_edge_bps {
        if expected_edge <= 0 {
            blockers.push(format!(
                "expected idle deposit edge {} must be positive",
                expected_edge
            ));
        }
    }

    let policy_plan = match build_initial_reserve_deposit_policy_plan(
        vault,
        initial_preview,
        policy_preflight,
        deposit_reserve,
        amount_raw,
        signer.pubkey(),
        signer.pubkey(),
        account_index,
    ) {
        Ok(plan) => Some(plan),
        Err(error) => {
            blockers.push(error.to_string());
            None
        }
    };
    let policy_transaction = if let Some(plan) = policy_plan.as_ref() {
        let mut policy_instructions = plan.pre_instructions.clone();
        policy_instructions.push(plan.instruction.clone());
        Some(build_signed_transaction(
            &rpc,
            signer.pubkey(),
            &policy_instructions,
            &lookup_table_accounts,
            &[&signer],
            "idle vault policy deposit",
            if blockers.is_empty() {
                None
            } else {
                Some("idle deposit simulation skipped because preflight blockers exist".to_owned())
            },
        )?)
    } else {
        None
    };

    let idle_decision_input = if options.execute {
        Some(IdleVaultDepositDecisionInput {
            target_reserve: deposit_reserve.to_owned(),
            target_market: Some(deposit_position.market.clone()),
            liquidity_mint: USDC_MINT.to_string(),
            amount_raw: amount_i64,
            idle_token_account: vault_usdc_ata.to_string(),
            idle_observed_slot: options.expected_idle_observed_slot.ok_or(
                "--deposit-idle-vault-reserve --execute requires --expected-idle-observed-slot",
            )?,
            idle_observed_at: options.expected_idle_observed_at.ok_or(
                "--deposit-idle-vault-reserve --execute requires --expected-idle-observed-at",
            )?,
            target_apy_bps: options.expected_target_apy_bps.ok_or(
                "--deposit-idle-vault-reserve --execute requires --expected-target-apy-bps",
            )?,
            estimated_edge_bps: options
                .expected_edge_bps
                .ok_or("--deposit-idle-vault-reserve --execute requires --expected-edge-bps")?,
            estimated_cost_lamports: 0,
        })
    } else {
        None
    };

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "vault": vault_json(vault),
                "vaultUsdcAta": vault_usdc_ata.to_string(),
                "chainReconcile": chain_reconcile_preview_json(initial_preview),
                "policyPreflight": policy_route_preflight_json(vault, &ReserveMove {
                    source_reserve: deposit_reserve.to_owned(),
                    target_reserve: deposit_reserve.to_owned(),
                }, policy_preflight),
                "preflightBlockers": blockers,
                "policyDeposit": policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": policy_transaction.as_ref().map(policy_transaction_json),
                "postConfirmReconcileReserves": idle_deposit_post_reconcile_reserves(options, deposit_reserve),
            }))?
        );
        return Ok(());
    }

    if !blockers.is_empty() {
        let blocker_reason = format!(
            "idle vault deposit preflight blocked: {}",
            blockers.join("; ")
        );
        let mut blocked_decision = None;
        let mut blocked_decision_skip_reason = None;
        if let Some(input) = idle_decision_input.clone() {
            let planned = client
                .record_idle_vault_deposit_decision(vault.id, input)
                .await?;
            match planned.status {
                PlanOutcomeStatus::Planned(decision) => {
                    let decision = if decision.status.is_terminal() {
                        decision
                    } else {
                        client
                            .advance_decision(
                                decision.id,
                                DecisionAdvance::Fail {
                                    reason: blocker_reason.clone(),
                                },
                            )
                            .await?
                    };
                    blocked_decision = Some(decision);
                }
                PlanOutcomeStatus::Skipped { reason } => {
                    blocked_decision_skip_reason = Some(reason.decision_reason().as_str());
                }
            }
        }
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "idle_vault_deposit_preflight_blocked",
                "writesDecision": blocked_decision.is_some(),
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
                "preflightBlockers": blockers,
                "decisionId": blocked_decision.as_ref().map(|decision| decision.id.as_i64()),
                "blockedDecision": blocked_decision.as_ref().map(idle_vault_deposit_decision_json),
                "blockedDecisionSkipReason": blocked_decision_skip_reason,
                "policyDeposit": policy_plan.as_ref().map(|plan| initial_deposit_policy_preview_json(&plan.preview)),
                "policyDepositTransaction": policy_transaction.as_ref().map(policy_transaction_json),
            }))?
        );
        return Err(blocker_reason.into());
    }

    let idle_decision_input =
        idle_decision_input.ok_or("idle vault deposit decision input was not built")?;
    let planned = client
        .record_idle_vault_deposit_decision(vault.id, idle_decision_input)
        .await?;
    let decision = match planned.status {
        PlanOutcomeStatus::Planned(decision) => decision,
        PlanOutcomeStatus::Skipped { reason } => {
            println!(
                "{}",
                serde_json::to_string_pretty(&json!({
                    "status": "idle_vault_deposit_not_planned",
                    "writesDecision": planned.decision_id.is_some(),
                    "sendsTransactions": false,
                    "skipReason": reason.decision_reason().as_str(),
                    "decisionId": planned.decision_id.map(|id| id.as_i64()),
                }))?
            );
            return Err("idle vault deposit was not planned".into());
        }
    };

    let policy_plan = policy_plan.ok_or("idle vault policy deposit plan was not built")?;
    let policy_transaction =
        policy_transaction.ok_or("idle vault policy deposit transaction was not built")?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartSimulation)
        .await?;
    if let Some(error) = &policy_transaction.simulation_error {
        client
            .advance_decision(
                decision.id,
                DecisionAdvance::Fail {
                    reason: format!("idle vault policy deposit simulation failed: {error}"),
                },
            )
            .await?;
        return Err(format!("idle vault policy deposit simulation failed: {error}").into());
    }
    client
        .advance_decision(decision.id, DecisionAdvance::SimulationReady)
        .await?;

    let submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = match rpc.send_and_confirm_transaction(&policy_transaction.transaction) {
        Ok(signature) => signature,
        Err(error) => {
            client
                .advance_decision(
                    decision.id,
                    DecisionAdvance::Fail {
                        reason: format!("idle vault policy deposit submission failed: {error}"),
                    },
                )
                .await?;
            return Err(format!("idle vault policy deposit submission failed: {error}").into());
        }
    };
    let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = signature.to_string();
    client
        .advance_decision(
            decision.id,
            DecisionAdvance::Submit {
                signature: signature.clone(),
                slot: Some(submitted_slot),
            },
        )
        .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartConfirmation)
        .await?;

    let post_confirm = async {
        let post_reconcile_reserves =
            idle_deposit_post_reconcile_reserves(options, deposit_reserve);
        let post_preview =
            load_chain_reconcile_preview(&options.rpc_url, vault, &post_reconcile_reserves)?;
        let post_reconcile_state = chain_preview_reconciled_state(&post_preview)?;
        let post_snapshot = client
            .reconcile_vault(vault.id, post_reconcile_state)
            .await?;
        let post_deposit_position = chain_position_for_reserve(&post_preview, deposit_reserve)?;
        let idle_after = client
            .record_current_idle_token_balance(CurrentIdleTokenBalance {
                vault_id: vault.id,
                mint: USDC_MINT.to_string(),
                amount_raw: i64::try_from(post_deposit_position.vault_liquidity_amount_raw)?,
                owner: vault.vault_pubkey.clone(),
                token_account: vault_usdc_ata.to_string(),
                observed_slot: post_preview.observed_slot,
                observed_at: Utc::now(),
                source_commitment: "confirmed".to_owned(),
                updated_at: Utc::now(),
            })
            .await?;
        let confirmed = client
            .advance_decision(
                decision.id,
                DecisionAdvance::Confirm {
                    slot: Some(confirmed_slot),
                    post_snapshot_id: Some(post_snapshot.id),
                },
            )
            .await?;
        Ok::<_, Box<dyn Error>>((
            post_reconcile_reserves,
            post_preview,
            post_snapshot,
            idle_after,
            confirmed,
        ))
    }
    .await;
    let (post_reconcile_reserves, post_preview, post_snapshot, idle_after, confirmed) =
        match post_confirm {
            Ok(value) => value,
            Err(error) => {
                let reason =
                    format!("idle vault policy deposit confirmed but reconcile failed: {error}");
                client
                    .advance_decision(
                        decision.id,
                        DecisionAdvance::Fail {
                            reason: reason.clone(),
                        },
                    )
                    .await?;
                return Err(reason.into());
            }
        };
    let repair = repair_idle_vault_deposit_partial_pull_history(
        client,
        vault,
        &confirmed,
        deposit_reserve,
        &deposit_position.market,
        &signature,
        confirmed_slot,
        amount_i64,
    )
    .await?;

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "idle_vault_deposit_executed",
            "writesDecision": true,
            "writesCurrentPositions": true,
            "sendsTransactions": true,
            "deposit": idle_vault_deposit_request_json(vault, deposit_reserve, &deposit_position, amount_raw, db_idle.as_ref(), options),
            "vault": vault_json(vault),
            "vaultUsdcAta": vault_usdc_ata.to_string(),
            "preparedDecision": idle_vault_deposit_decision_json(&decision),
            "confirmedDecision": idle_vault_deposit_decision_json(&confirmed),
            "policyDeposit": initial_deposit_policy_preview_json(&policy_plan.preview),
            "policyDepositTransaction": {
                "signature": signature,
                "submittedSlot": submitted_slot,
                "confirmedSlot": confirmed_slot,
                "simulationUnitsConsumed": policy_transaction.simulation_units_consumed,
                "transaction": transaction_packet_json(&policy_transaction.transaction_packet),
            },
            "reconciledSnapshotId": post_snapshot.id.as_i64(),
            "postConfirmReconcileReserves": post_reconcile_reserves,
            "postChainReconcile": chain_reconcile_preview_json(&post_preview),
            "idleVaultBalanceAfter": idle_balance_json(&idle_after),
            "partialPullRepair": repair,
        }))?
    );

    Ok(())
}

fn idle_deposit_post_reconcile_reserves(
    options: &CliOptions,
    deposit_reserve: &str,
) -> Vec<String> {
    let mut reserves = Vec::new();
    push_unique_string(&mut reserves, deposit_reserve.to_owned());
    for reserve in &options.reconcile_reserves {
        push_unique_string(&mut reserves, reserve.clone());
    }
    reserves
}

fn idle_vault_deposit_request_json(
    vault: &SelectedVault,
    deposit_reserve: &str,
    deposit_position: &ChainPositionSummary,
    amount_raw: u64,
    db_idle: Option<&CurrentIdleTokenBalance>,
    options: &CliOptions,
) -> Value {
    json!({
        "kind": "idle_vault_deposit",
        "sourceKind": "idle_vault",
        "reserve": deposit_reserve,
        "market": deposit_position.market,
        "liquidityMint": USDC_MINT.to_string(),
        "amountRaw": amount_raw.to_string(),
        "idleVaultLiquidityAmountRaw": amount_raw.to_string(),
        "idleTokenAccount": deposit_position.vault_liquidity_ata,
        "liveIdleAmountRaw": deposit_position.vault_liquidity_amount_raw.to_string(),
        "dbIdle": db_idle.map(idle_balance_json),
        "expected": {
            "idleTokenAccount": options.expected_idle_token_account,
            "idleObservedSlot": options.expected_idle_observed_slot,
            "idleObservedAt": options.expected_idle_observed_at.map(|value| value.to_rfc3339()),
            "liquidityMint": options.expected_liquidity_mint,
            "amountRaw": options.expected_amount_raw,
            "targetApyBps": options.expected_target_apy_bps,
            "edgeBps": options.expected_edge_bps,
        },
        "vaultId": vault.id.as_i64(),
    })
}

fn idle_balance_json(balance: &CurrentIdleTokenBalance) -> Value {
    json!({
        "vaultId": balance.vault_id.as_i64(),
        "mint": balance.mint,
        "amountRaw": balance.amount_raw.to_string(),
        "owner": balance.owner,
        "tokenAccount": balance.token_account,
        "observedSlot": balance.observed_slot,
        "observedAt": balance.observed_at,
        "sourceCommitment": balance.source_commitment,
        "updatedAt": balance.updated_at,
    })
}

fn idle_vault_deposit_decision_json(decision: &RebalanceDecision) -> Value {
    json!({
        "id": decision.id.as_i64(),
        "vaultId": decision.vault_id.as_i64(),
        "status": decision.status.as_str(),
        "decisionReason": decision.decision_reason.as_str(),
        "sourceReserve": decision.source_reserve,
        "targetReserve": decision.target_reserve,
        "liquidityMint": decision.liquidity_mint,
        "amountRaw": decision.amount_raw.map(|amount| amount.to_string()),
        "sourceApyBps": decision.source_apy_bps,
        "targetApyBps": decision.target_apy_bps,
        "estimatedEdgeBps": decision.estimated_edge_bps,
        "signature": decision.signature,
        "submittedSlot": decision.submitted_slot,
        "confirmedSlot": decision.confirmed_slot,
        "postSnapshotId": decision.post_snapshot_id.map(SnapshotId::as_i64),
        "executionPlan": decision.execution_plan,
    })
}

async fn repair_idle_vault_deposit_partial_pull_history(
    client: &NeonSqlClient,
    vault: &SelectedVault,
    decision: &RebalanceDecision,
    target_reserve: &str,
    target_market: &str,
    deposit_signature: &str,
    confirmed_slot: i64,
    planned_amount_raw: i64,
) -> Result<Value, Box<dyn Error>> {
    let mut tx = client.pool().begin().await?;
    let app_tables_exist: bool = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT to_regclass('loyal_yield.user_yield_position_deposits') IS NOT NULL
           AND to_regclass('loyal_yield.user_yield_positions') IS NOT NULL
           AND to_regclass('loyal_yield.user_yield_position_holding_events') IS NOT NULL
        "#,
    )
    .fetch_one(&mut *tx)
    .await?;

    let target_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, wallet, token_mint, vault_token_ata
        FROM loyal_yield.balance_sweep_targets
        WHERE settings = $1
          AND vault_index = $2
          AND vault_pubkey = $3
          AND token_mint = $4
        ORDER BY active DESC, last_seen_at DESC, id DESC
        LIMIT 1
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(&vault.vault_pubkey)
    .bind(USDC_MINT.to_string())
    .fetch_optional(&mut *tx)
    .await?;
    let Some(target_row) = target_row else {
        tx.commit().await?;
        return Ok(json!({
            "matchedPartialPullCount": 0,
            "matchedPartialPullAmountRaw": "0",
            "balanceSweepTargetFound": false,
            "appHistoryRepair": "skipped_no_balance_sweep_target",
        }));
    };
    let target_id: i64 = target_row.try_get("id")?;
    let wallet: String = target_row.try_get("wallet")?;
    let vault_token_ata: String = target_row.try_get("vault_token_ata")?;

    let execution_rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, amount_raw, signature
        FROM loyal_yield.balance_sweep_executions
        WHERE target_id = $1
          AND token_mint = $2
          AND COALESCE(destination_token_ata, destination_vault_ata) = $3
          AND decoded_evidence->>'status' = 'partial_executed_pull_top_up_blocked'
          AND decoded_evidence->>'idleVaultDepositDecisionId' IS NULL
        ORDER BY slot ASC, id ASC
        FOR UPDATE
        "#,
    )
    .bind(target_id)
    .bind(USDC_MINT.to_string())
    .bind(&vault_token_ata)
    .fetch_all(&mut *tx)
    .await?;

    let mut matched_ids = Vec::new();
    let mut matched_signatures = Vec::new();
    let mut matched_amount_raw = 0_i64;
    for row in execution_rows {
        let amount: i64 = row.try_get("amount_raw")?;
        if matched_amount_raw + amount > planned_amount_raw {
            break;
        }
        matched_amount_raw += amount;
        matched_ids.push(row.try_get::<i64, _>("id")?);
        matched_signatures.push(row.try_get::<String, _>("signature")?);
        if matched_amount_raw == planned_amount_raw {
            break;
        }
    }

    if matched_ids.is_empty() {
        tx.commit().await?;
        return Ok(json!({
            "matchedPartialPullCount": 0,
            "matchedPartialPullAmountRaw": "0",
            "balanceSweepTargetFound": true,
            "appHistoryRepair": "skipped_no_matching_partial_pull",
        }));
    }

    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.balance_sweep_executions
        SET
            decoded_evidence = COALESCE(decoded_evidence, '{}'::jsonb)
              || jsonb_build_object(
                    'previousStatus', decoded_evidence->>'status',
                    'status', 'partial_executed_pull_idle_vault_deposited',
                    'idleVaultDepositDecisionId', $2::text,
                    'kaminoDepositSignature', $3,
                    'kaminoDepositSlot', $4::text,
                    'idleVaultDepositAmountRaw', $5::text
                 ),
            decoded_at = now()
        WHERE id = ANY($1)
        "#,
    )
    .bind(&matched_ids)
    .bind(decision.id.as_i64())
    .bind(deposit_signature)
    .bind(confirmed_slot)
    .bind(planned_amount_raw)
    .execute(&mut *tx)
    .await?;

    let mut app_history_repair = json!("skipped_app_tables_missing");
    if app_tables_exist {
        app_history_repair = repair_idle_vault_deposit_app_history_in_tx(
            &mut tx,
            vault,
            target_reserve,
            target_market,
            &wallet,
            deposit_signature,
            confirmed_slot,
            matched_amount_raw,
            decision,
        )
        .await?;
    }

    tx.commit().await?;
    Ok(json!({
        "matchedPartialPullCount": matched_ids.len(),
        "matchedPartialPullIds": matched_ids,
        "matchedPartialPullSignatures": matched_signatures,
        "matchedPartialPullAmountRaw": matched_amount_raw.to_string(),
        "plannedAmountRaw": planned_amount_raw.to_string(),
        "balanceSweepTargetFound": true,
        "appHistoryRepair": app_history_repair,
    }))
}

async fn repair_idle_vault_deposit_app_history_in_tx(
    tx: &mut loyal_yield_orchestrator::sqlx::Transaction<
        '_,
        loyal_yield_orchestrator::sqlx::Postgres,
    >,
    vault: &SelectedVault,
    target_reserve: &str,
    target_market: &str,
    wallet: &str,
    deposit_signature: &str,
    confirmed_slot: i64,
    principal_delta_raw: i64,
    decision: &RebalanceDecision,
) -> Result<Value, Box<dyn Error>> {
    let deposit_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.user_yield_position_deposits (
            deposit_signature,
            policy_signature,
            confirmed_slot,
            wallet_address,
            smart_account_address,
            settings,
            vault_index,
            vault_pubkey,
            policy_id,
            policy_account,
            policy_seed,
            target_reserve,
            market,
            liquidity_mint,
            target_supply_apy_bps,
            deposit_mint,
            principal_amount_raw,
            confirmed_at,
            created_at
        )
        VALUES ($1, $1, $2, $3, $4, $5, $6, $4, $7, $8, $7, $9, $10, $11, $12, $11, $13, now(), now())
        ON CONFLICT (deposit_signature) DO NOTHING
        RETURNING id
        "#,
    )
    .bind(deposit_signature)
    .bind(confirmed_slot)
    .bind(wallet)
    .bind(&vault.vault_pubkey)
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(vault.policy_seed)
    .bind(&vault.policy_account)
    .bind(target_reserve)
    .bind(target_market)
    .bind(USDC_MINT.to_string())
    .bind(decision.target_apy_bps)
    .bind(principal_delta_raw)
    .fetch_optional(&mut **tx)
    .await?;

    let Some(deposit_row) = deposit_row else {
        return Ok(json!({
            "status": "duplicate_deposit_signature",
            "depositSignature": deposit_signature,
        }));
    };
    let deposit_id: i64 = deposit_row.try_get("id")?;
    let existing = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, current_amount_raw, principal_amount_raw, current_reserve, current_liquidity_mint
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1
          AND vault_index = $2
          AND wallet_address = $3
          AND status::text = 'active'
        ORDER BY updated_at DESC, id DESC
        LIMIT 1
        FOR UPDATE
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(wallet)
    .fetch_optional(&mut **tx)
    .await?;

    let observed_current_amount = decision.amount_raw.unwrap_or(principal_delta_raw);
    let (position_id, event_type, next_amount_raw, next_principal_raw, holding_delta_raw) =
        if let Some(existing) = existing {
            let position_id: i64 = existing.try_get("id")?;
            let current_amount_raw: i64 = existing.try_get("current_amount_raw")?;
            let principal_amount_raw: i64 = existing.try_get("principal_amount_raw")?;
            let current_reserve: String = existing.try_get("current_reserve")?;
            let current_liquidity_mint: String = existing.try_get("current_liquidity_mint")?;
            let same_current_holding = current_reserve == target_reserve
                && current_liquidity_mint == USDC_MINT.to_string();
            let next_amount_raw = if same_current_holding {
                observed_current_amount
            } else {
                current_amount_raw
            };
            let next_principal_raw = principal_amount_raw + principal_delta_raw;
            let holding_delta_raw = if same_current_holding {
                Some(next_amount_raw - current_amount_raw)
            } else {
                None
            };
            loyal_yield_orchestrator::sqlx::query(
                r#"
                UPDATE loyal_yield.user_yield_positions
                SET
                    deposit_mint = $2,
                    initial_liquidity_mint = $2,
                    initial_market = $3,
                    last_confirmed_slot = $4,
                    last_deposit_signature = $5,
                    policy_account = $6,
                    policy_id = $7,
                    policy_seed = $7,
                    principal_amount_raw = $8,
                    smart_account_address = $9,
                    status = 'active'::loyal_yield.yield_position_status,
                    updated_at = now(),
                    vault_pubkey = $9,
                    wallet_address = $10
                WHERE id = $1
                "#,
            )
            .bind(position_id)
            .bind(USDC_MINT.to_string())
            .bind(target_market)
            .bind(confirmed_slot)
            .bind(deposit_signature)
            .bind(&vault.policy_account)
            .bind(vault.policy_seed)
            .bind(next_principal_raw)
            .bind(&vault.vault_pubkey)
            .bind(wallet)
            .execute(&mut **tx)
            .await?;
            (
                position_id,
                "deposit_top_up",
                next_amount_raw,
                next_principal_raw,
                holding_delta_raw,
            )
        } else {
            let row = loyal_yield_orchestrator::sqlx::query(
                r#"
                INSERT INTO loyal_yield.user_yield_positions (
                    wallet_address,
                    smart_account_address,
                    settings,
                    vault_index,
                    vault_pubkey,
                    policy_id,
                    policy_account,
                    policy_seed,
                    initial_reserve,
                    initial_market,
                    initial_liquidity_mint,
                    initial_supply_apy_bps,
                    deposit_mint,
                    principal_amount_raw,
                    current_reserve,
                    current_market,
                    current_liquidity_mint,
                    current_amount_raw,
                    current_observed_slot,
                    current_observed_at,
                    first_deposit_signature,
                    last_deposit_signature,
                    last_confirmed_slot,
                    status,
                    created_at,
                    updated_at
                )
                VALUES ($1, $2, $3, $4, $2, $5, $6, $5, $7, $8, $9, $10, $9, $11, $7, $8, $9, $12, $13, now(), $14, $14, $13, 'active'::loyal_yield.yield_position_status, now(), now())
                RETURNING id
                "#,
            )
            .bind(wallet)
            .bind(&vault.vault_pubkey)
            .bind(&vault.settings)
            .bind(vault.vault_index)
            .bind(vault.policy_seed)
            .bind(&vault.policy_account)
            .bind(target_reserve)
            .bind(target_market)
            .bind(USDC_MINT.to_string())
            .bind(decision.target_apy_bps)
            .bind(principal_delta_raw)
            .bind(observed_current_amount)
            .bind(confirmed_slot)
            .bind(deposit_signature)
            .fetch_one(&mut **tx)
            .await?;
            (
                row.try_get("id")?,
                "deposit_initialized",
                observed_current_amount,
                principal_delta_raw,
                Some(principal_delta_raw),
            )
        };

    let event_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        INSERT INTO loyal_yield.user_yield_position_holding_events (
            position_id,
            event_type,
            reserve,
            market,
            liquidity_mint,
            amount_raw,
            principal_delta_raw,
            holding_delta_raw,
            observed_slot,
            observed_at,
            source_signature,
            source_deposit_id,
            source_rebalance_decision_id,
            created_at
        )
        VALUES ($1, $2::text::loyal_yield.user_yield_holding_event_type, $3, $4, $5, $6, $7, $8, $9, now(), $10, $11, $12, now())
        RETURNING id
        "#,
    )
    .bind(position_id)
    .bind(event_type)
    .bind(target_reserve)
    .bind(target_market)
    .bind(USDC_MINT.to_string())
    .bind(next_amount_raw)
    .bind(principal_delta_raw)
    .bind(holding_delta_raw)
    .bind(confirmed_slot)
    .bind(deposit_signature)
    .bind(deposit_id)
    .bind(decision.id.as_i64())
    .fetch_one(&mut **tx)
    .await?;
    let event_id: i64 = event_row.try_get("id")?;

    loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.user_yield_positions
        SET
            current_amount_raw = $2,
            current_liquidity_mint = $3,
            current_market = $4,
            current_observed_at = now(),
            current_observed_slot = $5,
            current_reserve = $6,
            last_holding_event_id = $7,
            last_confirmed_slot = $5,
            last_deposit_signature = $8,
            principal_amount_raw = $9,
            status = 'active'::loyal_yield.yield_position_status,
            updated_at = now()
        WHERE id = $1
        "#,
    )
    .bind(position_id)
    .bind(next_amount_raw)
    .bind(USDC_MINT.to_string())
    .bind(target_market)
    .bind(confirmed_slot)
    .bind(target_reserve)
    .bind(event_id)
    .bind(deposit_signature)
    .bind(next_principal_raw)
    .execute(&mut **tx)
    .await?;

    Ok(json!({
        "status": "repaired",
        "positionId": position_id,
        "depositId": deposit_id,
        "holdingEventId": event_id,
        "principalDeltaRaw": principal_delta_raw.to_string(),
        "nextPrincipalRaw": next_principal_raw.to_string(),
        "nextAmountRaw": next_amount_raw.to_string(),
    }))
}

async fn deactivate_vault_policy_after_full_withdraw(
    client: &NeonSqlClient,
    vault: &SelectedVault,
) -> Result<Value, Box<dyn Error>> {
    let mut tx = client.pool().begin().await?;
    let policy_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.route_policies
        SET active = false, last_seen_at = now()
        WHERE policy_account = $1
        RETURNING id, active
        "#,
    )
    .bind(&vault.policy_account)
    .fetch_one(&mut *tx)
    .await?;
    let setup_policy_row = if let Some(setup_policy_account) = vault.setup_policy_account.as_ref() {
        Some(
            loyal_yield_orchestrator::sqlx::query(
                r#"
                UPDATE loyal_yield.route_policies
                SET active = false, last_seen_at = now()
                WHERE policy_account = $1
                RETURNING id, active
                "#,
            )
            .bind(setup_policy_account)
            .fetch_one(&mut *tx)
            .await?,
        )
    } else {
        None
    };
    let vault_row = loyal_yield_orchestrator::sqlx::query(
        r#"
        UPDATE loyal_yield.managed_vaults
        SET active = false, last_seen_at = now()
        WHERE id = $1
        RETURNING id, active
        "#,
    )
    .bind(vault.id.as_i64())
    .fetch_one(&mut *tx)
    .await?;
    tx.commit().await?;

    Ok(json!({
        "policyId": policy_row.try_get::<i64, _>("id")?,
        "policyActive": policy_row.try_get::<bool, _>("active")?,
        "setupPolicyId": match setup_policy_row.as_ref() {
            Some(row) => Value::from(row.try_get::<i64, _>("id")?),
            None => Value::Null,
        },
        "setupPolicyActive": match setup_policy_row.as_ref() {
            Some(row) => Value::from(row.try_get::<bool, _>("active")?),
            None => Value::Null,
        },
        "vaultId": vault_row.try_get::<i64, _>("id")?,
        "vaultActive": vault_row.try_get::<bool, _>("active")?,
    }))
}

async fn run_reconcile_current_positions_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
) -> Result<(), Box<dyn Error>> {
    let snapshot = client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(preview)?)
        .await?;
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "current_positions_reconciled",
            "writesDecision": false,
            "writesCurrentPositions": true,
            "sendsTransactions": false,
            "execute": options.execute,
            "vault": vault_json(vault),
            "requestedReserves": options.reconcile_reserves,
            "reconciledReserveCount": preview.positions.len(),
            "reconciledSnapshotId": snapshot.id.as_i64(),
            "chainReconcile": chain_reconcile_preview_json(preview),
        }))?
    );
    Ok(())
}

async fn run_full_reserve_withdraw_flow(
    options: &CliOptions,
    client: &NeonSqlClient,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    withdraw_reserve: &str,
) -> Result<(), Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let lookup_table_pubkeys = lookup_table_pubkeys_from_options(options)?;
    let lookup_table_accounts = load_address_lookup_table_accounts(&rpc, &lookup_table_pubkeys)?;
    let signer = policy_keypair_from_env()?;
    let authority_signer = solana_testing_keypair_from_env()?;
    let authority_pubkey = Pubkey::from_str(&vault.authority)?;
    if authority_signer.pubkey() != authority_pubkey {
        return Err(format!(
            "SOLANA_TESTING_PK pubkey {} does not match policy authority {}",
            authority_signer.pubkey(),
            authority_pubkey
        )
        .into());
    }
    let settings_pubkey = Pubkey::from_str(&vault.settings)?;
    let policy_account_pubkey = Pubkey::from_str(&vault.policy_account)?;
    let setup_policy_account_pubkey = vault
        .setup_policy_account
        .as_deref()
        .map(Pubkey::from_str)
        .transpose()?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let withdraw = chain_position_for_reserve(preview, withdraw_reserve)?;
    let withdraw_obligation_pubkey = Pubkey::from_str(&withdraw.obligation)?;
    let withdraw_reserve_pubkey = Pubkey::from_str(&withdraw.reserve)?;
    let withdraw_market_pubkey = Pubkey::from_str(&withdraw.market)?;
    let wallet_usdc_ata =
        derive_associated_token_address(&authority_signer.pubkey(), &USDC_MINT, &spl_token::ID);
    let vault_usdc_ata = derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let authority_account_before = load_account_proof(&rpc, &authority_signer.pubkey())?;
    let (wallet_usdc_before_raw, wallet_usdc_before_exists) =
        load_spl_token_account_amount(&rpc, &wallet_usdc_ata, &USDC_MINT)?;
    let vault_usdc_ata_before = load_account_proof(&rpc, &vault_usdc_ata)?;
    let policy_account_before = load_account_proof(&rpc, &policy_account_pubkey)?;
    let setup_policy_account_before = setup_policy_account_pubkey
        .as_ref()
        .map(|pubkey| load_account_proof(&rpc, pubkey))
        .transpose()?;
    let vault_account_before = load_account_proof(&rpc, &vault_pubkey)?;
    let obligation_before = load_obligation_account_proof(
        &rpc,
        &withdraw_obligation_pubkey,
        &vault_pubkey,
        &withdraw_market_pubkey,
        &withdraw_reserve_pubkey,
    )?;

    let mut blockers = Vec::new();
    if !withdraw.obligation_exists {
        blockers.push(format!(
            "withdraw obligation account {} does not exist for reserve {}",
            withdraw.obligation, withdraw.reserve
        ));
    }
    if withdraw.amount_raw == 0 {
        blockers.push(format!(
            "withdraw obligation account {} has zero deposited amount for reserve {}",
            withdraw.obligation, withdraw.reserve
        ));
    }
    if !withdraw.vault_liquidity_token_account_exists {
        blockers.push(format!(
            "vault USDC ATA {} does not exist",
            withdraw.vault_liquidity_ata
        ));
    }
    if !policy_account_before.exists {
        blockers.push(format!(
            "policy account {} does not exist",
            vault.policy_account
        ));
    }

    let policy_plan = match build_full_main_usdc_withdraw_policy_plan(
        vault,
        preview,
        policy_preflight,
        signer.pubkey(),
        account_index,
        withdraw_reserve,
    ) {
        Ok(plan) => Some(plan),
        Err(error) => {
            blockers.push(error.to_string());
            None
        }
    };
    let withdraw_transaction = if let Some(plan) = policy_plan.as_ref() {
        let mut instructions = plan.pre_instructions.clone();
        instructions.push(plan.instruction.clone());
        Some(build_signed_transaction(
            &rpc,
            signer.pubkey(),
            &instructions,
            &lookup_table_accounts,
            &[&signer],
            "full reserve USDC policy withdraw",
            if blockers.is_empty() {
                None
            } else {
                Some("withdraw simulation skipped because preflight blockers exist".to_owned())
            },
        )?)
    } else {
        None
    };
    let wallet_recovery_transaction = Some(build_vault_usdc_recovery_transaction(
        &rpc,
        &lookup_table_accounts,
        settings_pubkey,
        &authority_signer,
        vault_pubkey,
        account_index,
        wallet_usdc_ata,
        vault_usdc_ata,
        withdraw.amount_raw,
        Some("wallet recovery simulation requires the Kamino withdraw to land first".to_owned()),
    )?);
    let policy_close_instruction = remove_policy_instruction(
        settings_pubkey,
        authority_signer.pubkey(),
        policy_account_pubkey,
    );
    let policy_close_transaction = Some(build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        policy_close_instruction,
        &lookup_table_accounts,
        &authority_signer,
        "full withdraw policy close",
        if blockers.is_empty() {
            None
        } else {
            Some("policy close simulation skipped because preflight blockers exist".to_owned())
        },
    )?);
    let setup_policy_close_transaction =
        if let (Some(setup_policy_pubkey), Some(setup_policy_before)) = (
            setup_policy_account_pubkey.as_ref(),
            setup_policy_account_before.as_ref(),
        ) {
            if setup_policy_before.exists {
                let setup_policy_close_instruction = remove_policy_instruction(
                    settings_pubkey,
                    authority_signer.pubkey(),
                    *setup_policy_pubkey,
                );
                Some(build_policy_transaction(
                    &rpc,
                    authority_signer.pubkey(),
                    setup_policy_close_instruction,
                    &lookup_table_accounts,
                    &authority_signer,
                    "full withdraw setup policy close",
                    if blockers.is_empty() {
                        Some(
                            "setup policy close simulation waits until the route policy close lands"
                                .to_owned(),
                        )
                    } else {
                        Some(
                            "setup policy close simulation skipped because preflight blockers exist"
                                .to_owned(),
                        )
                    },
                )?)
            } else {
                None
            }
        } else {
            None
        };
    dedup_strings_in_place(&mut blockers);

    if !options.execute {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "full_withdraw_reserve_dry_run",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "withdraw": {
                    "reserve": withdraw.reserve,
                    "market": withdraw.market,
                    "liquidityMint": USDC_MINT.to_string(),
                    "amountRaw": withdraw.amount_raw.to_string(),
                    "amountSemantics": "kamino_obligation_collateral_deposited_amount",
                },
                "vault": vault_json(vault),
                "chainReconcile": chain_reconcile_preview_json(preview),
                "policyPreflight": policy_route_preflight_json(vault, &ReserveMove {
                    source_reserve: KAMINO_MAIN_USDC_RESERVE.to_string(),
                    target_reserve: KAMINO_PRIME_USDC_RESERVE.to_owned(),
                }, policy_preflight),
                "preflightBlockers": blockers,
                "rentCleanupProof": {
                    "vaultBefore": account_proof_json(&vault_account_before),
                    "authorityBefore": account_proof_json(&authority_account_before),
                    "vaultUsdcAtaBefore": account_proof_json(&vault_usdc_ata_before),
                    "policyBefore": account_proof_json(&policy_account_before),
                    "setupPolicyBefore": setup_policy_account_before.as_ref().map(account_proof_json),
                    "withdrawObligationBefore": obligation_account_proof_json(&obligation_before),
                    "afterAvailable": false,
                    "expectedRefundRecipient": vault.vault_pubkey,
                },
                "walletRecovery": {
                    "wallet": authority_signer.pubkey().to_string(),
                    "walletUsdcAta": wallet_usdc_ata.to_string(),
                        "walletUsdcBeforeRaw": wallet_usdc_before_raw.to_string(),
                        "walletUsdcBeforeExists": wallet_usdc_before_exists,
                        "estimatedTransferAmountRaw": withdraw.amount_raw.to_string(),
                        "cleanupSigner": authority_signer.pubkey().to_string(),
                    },
                "policyWithdraw": policy_plan.as_ref().map(|plan| full_withdraw_policy_preview_json(&plan.preview)),
                "policyWithdrawTransaction": withdraw_transaction.as_ref().map(policy_transaction_json),
                "walletRecoveryTransaction": wallet_recovery_transaction.as_ref().map(policy_transaction_json),
                "policyClose": {
                        "policyAccount": vault.policy_account,
                        "settings": vault.settings,
                        "authority": authority_signer.pubkey().to_string(),
                        "kind": "squads_execute_settings_transaction_sync_policy_remove",
                    },
                "policyCloseTransaction": policy_close_transaction.as_ref().map(policy_transaction_json),
                "setupPolicyClose": setup_policy_account_before.as_ref().map(|account| json!({
                    "policyAccount": vault.setup_policy_account,
                    "settings": vault.settings,
                    "authority": authority_signer.pubkey().to_string(),
                    "kind": "squads_execute_settings_transaction_sync_policy_remove",
                    "policyExists": account.exists,
                })),
                "setupPolicyCloseTransaction": setup_policy_close_transaction.as_ref().map(policy_transaction_json),
            }))?
        );
        return Ok(());
    }

    if !blockers.is_empty() {
        println!(
            "{}",
            serde_json::to_string_pretty(&json!({
                "status": "full_withdraw_reserve_preflight_blocked",
                "writesDecision": false,
                "writesCurrentPositions": false,
                "sendsTransactions": false,
                "preflightBlockers": blockers,
                "rentCleanupProof": {
                    "vaultBefore": account_proof_json(&vault_account_before),
                    "authorityBefore": account_proof_json(&authority_account_before),
                    "vaultUsdcAtaBefore": account_proof_json(&vault_usdc_ata_before),
                    "policyBefore": account_proof_json(&policy_account_before),
                    "setupPolicyBefore": setup_policy_account_before.as_ref().map(account_proof_json),
                    "withdrawObligationBefore": obligation_account_proof_json(&obligation_before),
                },
                "policyWithdraw": policy_plan.as_ref().map(|plan| full_withdraw_policy_preview_json(&plan.preview)),
                "policyWithdrawTransaction": withdraw_transaction.as_ref().map(policy_transaction_json),
                "walletRecoveryTransaction": wallet_recovery_transaction.as_ref().map(policy_transaction_json),
                "policyCloseTransaction": policy_close_transaction.as_ref().map(policy_transaction_json),
                "setupPolicyCloseTransaction": setup_policy_close_transaction.as_ref().map(policy_transaction_json),
            }))?
        );
        return Err("full reserve withdraw preflight blocked before live submit".into());
    }
    let policy_plan = policy_plan.ok_or("full withdraw plan was not built")?;
    let withdraw_transaction =
        withdraw_transaction.ok_or("full withdraw transaction was not built")?;
    if let Some(error) = &withdraw_transaction.simulation_error {
        return Err(format!("full reserve withdraw simulation failed: {error}").into());
    }

    let submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = rpc.send_and_confirm_transaction(&withdraw_transaction.transaction)?;
    let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    let (vault_usdc_after_withdraw_raw, vault_usdc_after_withdraw_exists) =
        load_spl_token_account_amount(&rpc, &vault_usdc_ata, &USDC_MINT)?;
    if !vault_usdc_after_withdraw_exists {
        return Err(format!(
            "vault USDC ATA {} is missing after Kamino withdraw",
            vault_usdc_ata
        )
        .into());
    }
    let wallet_recovery_transaction = build_vault_usdc_recovery_transaction(
        &rpc,
        &lookup_table_accounts,
        settings_pubkey,
        &authority_signer,
        vault_pubkey,
        account_index,
        wallet_usdc_ata,
        vault_usdc_ata,
        vault_usdc_after_withdraw_raw,
        None,
    )?;
    if let Some(error) = &wallet_recovery_transaction.simulation_error {
        return Err(format!("full withdraw wallet recovery simulation failed: {error}").into());
    }
    let wallet_recovery_submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let wallet_recovery_signature =
        rpc.send_and_confirm_transaction(&wallet_recovery_transaction.transaction)?;
    let wallet_recovery_confirmed_slot = i64::try_from(rpc.get_slot()?)?;

    let policy_close_instruction = remove_policy_instruction(
        settings_pubkey,
        authority_signer.pubkey(),
        policy_account_pubkey,
    );
    let policy_close_transaction = build_policy_transaction(
        &rpc,
        authority_signer.pubkey(),
        policy_close_instruction,
        &lookup_table_accounts,
        &authority_signer,
        "full withdraw policy close",
        None,
    )?;
    if let Some(error) = &policy_close_transaction.simulation_error {
        return Err(format!("full withdraw policy close simulation failed: {error}").into());
    }
    let policy_close_submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let policy_close_signature =
        rpc.send_and_confirm_transaction(&policy_close_transaction.transaction)?;
    let policy_close_confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    let setup_policy_close_result = if let (Some(setup_policy_pubkey), Some(setup_policy_before)) = (
        setup_policy_account_pubkey.as_ref(),
        setup_policy_account_before.as_ref(),
    ) {
        if setup_policy_before.exists {
            let setup_policy_close_instruction = remove_policy_instruction(
                settings_pubkey,
                authority_signer.pubkey(),
                *setup_policy_pubkey,
            );
            let setup_policy_close_transaction = build_policy_transaction(
                &rpc,
                authority_signer.pubkey(),
                setup_policy_close_instruction,
                &lookup_table_accounts,
                &authority_signer,
                "full withdraw setup policy close",
                None,
            )?;
            if let Some(error) = &setup_policy_close_transaction.simulation_error {
                return Err(
                    format!("full withdraw setup policy close simulation failed: {error}").into(),
                );
            }
            let submitted_slot = i64::try_from(rpc.get_slot()?)?;
            let signature =
                rpc.send_and_confirm_transaction(&setup_policy_close_transaction.transaction)?;
            let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
            Some((
                signature,
                submitted_slot,
                confirmed_slot,
                setup_policy_close_transaction,
            ))
        } else {
            None
        }
    } else {
        None
    };

    let post_preview = load_chain_reconcile_preview(
        &options.rpc_url,
        vault,
        &preview
            .positions
            .iter()
            .map(|position| position.reserve.clone())
            .collect::<Vec<_>>(),
    )?;
    let vault_account_after = load_account_proof(&rpc, &vault_pubkey)?;
    let obligation_after = load_obligation_account_proof(
        &rpc,
        &withdraw_obligation_pubkey,
        &vault_pubkey,
        &withdraw_market_pubkey,
        &withdraw_reserve_pubkey,
    )?;
    let authority_account_after = load_account_proof(&rpc, &authority_signer.pubkey())?;
    let (wallet_usdc_after_raw, wallet_usdc_after_exists) =
        load_spl_token_account_amount(&rpc, &wallet_usdc_ata, &USDC_MINT)?;
    let vault_usdc_ata_after = load_account_proof(&rpc, &vault_usdc_ata)?;
    let policy_account_after = load_account_proof(&rpc, &policy_account_pubkey)?;
    let setup_policy_account_after = setup_policy_account_pubkey
        .as_ref()
        .map(|pubkey| load_account_proof(&rpc, pubkey))
        .transpose()?;
    let snapshot = client
        .reconcile_vault(vault.id, chain_preview_reconciled_state(&post_preview)?)
        .await?;
    let inactive = deactivate_vault_policy_after_full_withdraw(client, vault).await?;

    let rent_refund_lamports =
        i128::from(vault_account_after.lamports) - i128::from(vault_account_before.lamports);
    let authority_lamports_delta = i128::from(authority_account_after.lamports)
        - i128::from(authority_account_before.lamports);
    let closed_obligation_lamports = i128::from(obligation_before.account.lamports);
    let closed_policy_lamports = i128::from(policy_account_before.lamports);
    let closed_setup_policy_lamports = setup_policy_account_before
        .as_ref()
        .map(|account| i128::from(account.lamports))
        .unwrap_or(0);
    let closed_vault_usdc_ata_lamports = i128::from(vault_usdc_ata_before.lamports);
    let wallet_usdc_delta = i128::from(wallet_usdc_after_raw) - i128::from(wallet_usdc_before_raw);
    let all_tracked_positions_zero = post_preview
        .positions
        .iter()
        .all(|position| position.amount_raw == 0);
    let all_tracked_obligations_closed = post_preview
        .positions
        .iter()
        .all(|position| !position.obligation_exists);
    let policy_withdraw_transaction_json = json!({
        "signature": signature.to_string(),
        "submittedSlot": submitted_slot,
        "confirmedSlot": confirmed_slot,
        "simulationUnitsConsumed": withdraw_transaction.simulation_units_consumed,
        "transaction": transaction_packet_json(&withdraw_transaction.transaction_packet),
    });
    let wallet_recovery_json = json!({
        "wallet": authority_signer.pubkey().to_string(),
        "cleanupSigner": authority_signer.pubkey().to_string(),
        "walletUsdcAta": wallet_usdc_ata.to_string(),
        "walletUsdcBeforeRaw": wallet_usdc_before_raw.to_string(),
        "walletUsdcBeforeExists": wallet_usdc_before_exists,
        "walletUsdcAfterRaw": wallet_usdc_after_raw.to_string(),
        "walletUsdcAfterExists": wallet_usdc_after_exists,
        "walletUsdcDeltaRaw": wallet_usdc_delta.to_string(),
        "vaultUsdcAfterWithdrawRaw": vault_usdc_after_withdraw_raw.to_string(),
        "vaultUsdcAtaClosed": vault_usdc_ata_before.exists && !vault_usdc_ata_after.exists,
    });
    let wallet_recovery_transaction_json = json!({
        "signature": wallet_recovery_signature.to_string(),
        "submittedSlot": wallet_recovery_submitted_slot,
        "confirmedSlot": wallet_recovery_confirmed_slot,
        "simulationUnitsConsumed": wallet_recovery_transaction.simulation_units_consumed,
        "transaction": transaction_packet_json(&wallet_recovery_transaction.transaction_packet),
    });
    let policy_close_json = json!({
        "policyAccount": vault.policy_account,
        "settings": vault.settings,
        "authority": authority_signer.pubkey().to_string(),
        "kind": "squads_execute_settings_transaction_sync_policy_remove",
        "policyClosed": policy_account_before.exists && !policy_account_after.exists,
    });
    let policy_close_transaction_json = json!({
        "signature": policy_close_signature.to_string(),
        "submittedSlot": policy_close_submitted_slot,
        "confirmedSlot": policy_close_confirmed_slot,
        "simulationUnitsConsumed": policy_close_transaction.simulation_units_consumed,
        "transaction": transaction_packet_json(&policy_close_transaction.transaction_packet),
    });
    let setup_policy_close_json = match setup_policy_account_before.as_ref() {
        Some(before) => json!({
            "policyAccount": vault.setup_policy_account,
            "settings": vault.settings,
            "authority": authority_signer.pubkey().to_string(),
            "kind": "squads_execute_settings_transaction_sync_policy_remove",
            "policyClosed": setup_policy_account_after
                .as_ref()
                .map(|after| before.exists && !after.exists)
                .unwrap_or(false),
        }),
        None => Value::Null,
    };
    let setup_policy_close_transaction_json = match setup_policy_close_result.as_ref() {
        Some((signature, submitted_slot, confirmed_slot, transaction)) => json!({
            "signature": signature.to_string(),
            "submittedSlot": submitted_slot,
            "confirmedSlot": confirmed_slot,
            "simulationUnitsConsumed": transaction.simulation_units_consumed,
            "transaction": transaction_packet_json(&transaction.transaction_packet),
        }),
        None => Value::Null,
    };
    let position_cleanup_proof_json = json!({
        "allTrackedPositionsZero": all_tracked_positions_zero,
        "allTrackedObligationsClosed": all_tracked_obligations_closed,
        "inactiveRows": inactive,
    });
    let rent_cleanup_proof_json = json!({
        "vaultBefore": account_proof_json(&vault_account_before),
        "vaultAfter": account_proof_json(&vault_account_after),
        "authorityBefore": account_proof_json(&authority_account_before),
        "authorityAfter": account_proof_json(&authority_account_after),
        "authorityLamportsDelta": authority_lamports_delta.to_string(),
        "vaultUsdcAtaBefore": account_proof_json(&vault_usdc_ata_before),
        "vaultUsdcAtaAfter": account_proof_json(&vault_usdc_ata_after),
        "policyBefore": account_proof_json(&policy_account_before),
        "policyAfter": account_proof_json(&policy_account_after),
        "policyClosed": policy_account_before.exists && !policy_account_after.exists,
        "setupPolicyBefore": setup_policy_account_before.as_ref().map(account_proof_json),
        "setupPolicyAfter": setup_policy_account_after.as_ref().map(account_proof_json),
        "setupPolicyClosed": setup_policy_account_before
            .as_ref()
            .zip(setup_policy_account_after.as_ref())
            .map(|(before, after)| before.exists && !after.exists),
        "withdrawObligationBefore": obligation_account_proof_json(&obligation_before),
        "withdrawObligationAfter": obligation_account_proof_json(&obligation_after),
        "withdrawObligationClosed": obligation_before.account.exists && !obligation_after.account.exists,
        "rentRefundLamports": rent_refund_lamports.to_string(),
        "closedObligationLamports": closed_obligation_lamports.to_string(),
        "closedPolicyLamports": closed_policy_lamports.to_string(),
        "closedSetupPolicyLamports": closed_setup_policy_lamports.to_string(),
        "closedVaultUsdcAtaLamports": closed_vault_usdc_ata_lamports.to_string(),
        "refundRecipient": vault.vault_pubkey,
        "refundAtLeastClosedObligationLamports": rent_refund_lamports >= closed_obligation_lamports,
    });

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "full_withdraw_reserve_executed",
            "writesDecision": false,
            "writesCurrentPositions": true,
            "sendsTransactions": true,
            "withdraw": {
                "reserve": withdraw.reserve,
                "market": withdraw.market,
                "liquidityMint": USDC_MINT.to_string(),
                "amountRaw": withdraw.amount_raw.to_string(),
                "amountSemantics": "kamino_obligation_collateral_deposited_amount",
            },
            "vault": vault_json(vault),
            "policyWithdraw": full_withdraw_policy_preview_json(&policy_plan.preview),
            "policyWithdrawTransaction": policy_withdraw_transaction_json,
            "walletRecovery": wallet_recovery_json,
            "walletRecoveryTransaction": wallet_recovery_transaction_json,
            "policyClose": policy_close_json,
            "policyCloseTransaction": policy_close_transaction_json,
            "setupPolicyClose": setup_policy_close_json,
            "setupPolicyCloseTransaction": setup_policy_close_transaction_json,
            "reconciledSnapshotId": snapshot.id.as_i64(),
            "postChainReconcile": chain_reconcile_preview_json(&post_preview),
            "positionCleanupProof": position_cleanup_proof_json,
            "rentCleanupProof": rent_cleanup_proof_json,
        }))?
    );

    Ok(())
}

fn build_vault_usdc_recovery_transaction(
    rpc: &RpcClient,
    lookup_table_accounts: &[AddressLookupTableAccount],
    settings: Pubkey,
    authority_signer: &dyn Signer,
    vault_pubkey: Pubkey,
    account_index: u8,
    wallet_usdc_ata: Pubkey,
    vault_usdc_ata: Pubkey,
    amount_raw: u64,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    let mut inner_instructions = Vec::new();
    if amount_raw > 0 {
        inner_instructions.push(spl_token::instruction::transfer_checked(
            &spl_token::ID,
            &vault_usdc_ata,
            &USDC_MINT,
            &wallet_usdc_ata,
            &vault_pubkey,
            &[],
            amount_raw,
            6,
        )?);
    }
    inner_instructions.push(spl_token::instruction::close_account(
        &spl_token::ID,
        &vault_usdc_ata,
        &authority_signer.pubkey(),
        &vault_pubkey,
        &[],
    )?);

    let mut transaction_accounts = Vec::new();
    let compiled_instructions = inner_instructions
        .into_iter()
        .map(|instruction| compile_squads_inner_instruction(&mut transaction_accounts, instruction))
        .collect::<Vec<_>>();
    let recovery_instruction = execute_sync_transaction_instruction(
        settings,
        authority_signer.pubkey(),
        account_index,
        compiled_instructions,
        transaction_accounts,
    );
    let instructions = vec![
        create_associated_token_account_idempotent_instruction(
            authority_signer.pubkey(),
            authority_signer.pubkey(),
            USDC_MINT,
            spl_token::ID,
        ),
        recovery_instruction,
    ];

    build_signed_transaction(
        rpc,
        authority_signer.pubkey(),
        &instructions,
        lookup_table_accounts,
        &[authority_signer],
        "full withdraw vault USDC recovery",
        simulation_skip_reason,
    )
}

fn build_policy_transaction(
    rpc: &RpcClient,
    payer: Pubkey,
    instruction: Instruction,
    lookup_table_accounts: &[AddressLookupTableAccount],
    signer: &dyn Signer,
    operation_label: &str,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    build_signed_transaction(
        rpc,
        payer,
        &[instruction],
        lookup_table_accounts,
        &[signer],
        operation_label,
        simulation_skip_reason,
    )
}

fn build_signed_transaction(
    rpc: &RpcClient,
    payer: Pubkey,
    instructions: &[Instruction],
    lookup_table_accounts: &[AddressLookupTableAccount],
    signers: &[&dyn Signer],
    operation_label: &str,
    simulation_skip_reason: Option<String>,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    build_signed_transaction_for_mode(
        rpc,
        payer,
        instructions,
        lookup_table_accounts,
        signers,
        operation_label,
        simulation_skip_reason,
        AltInstructionMode::RejectProvisioning,
    )
}

fn build_signed_transaction_for_mode(
    rpc: &RpcClient,
    payer: Pubkey,
    instructions: &[Instruction],
    lookup_table_accounts: &[AddressLookupTableAccount],
    signers: &[&dyn Signer],
    operation_label: &str,
    simulation_skip_reason: Option<String>,
    alt_instruction_mode: AltInstructionMode,
) -> Result<PolicyTransactionBuild, Box<dyn Error>> {
    guard_lookup_table_mutations(instructions, alt_instruction_mode, operation_label)?;
    let blockhash = rpc.get_latest_blockhash()?;
    let transaction = compile_versioned_transaction(
        payer,
        instructions,
        lookup_table_accounts,
        blockhash,
        signers,
    )?;
    let transaction_packet = transaction_packet_summary(&transaction, lookup_table_accounts)?;
    let best_case_single_lookup_table_packet =
        best_case_single_lookup_table_packet_summary(payer, instructions, blockhash, signers)?;
    let packet_error = if transaction_packet.fits_packet_data_size {
        None
    } else {
        Some(format!(
            "{operation_label} transaction is too large for one packet: {} > {} bytes",
            transaction_packet.packet_size_bytes, transaction_packet.packet_data_size_bytes
        ))
    };
    let simulation_skipped_reason = if let Some(reason) = simulation_skip_reason {
        Some(reason)
    } else if !transaction_packet.fits_packet_data_size {
        Some(format!(
            "serialized v0 transaction is {} bytes; Solana packet limit is {} bytes",
            transaction_packet.packet_size_bytes, transaction_packet.packet_data_size_bytes
        ))
    } else {
        None
    };
    let simulation = if simulation_skipped_reason.is_none() {
        Some(rpc.simulate_transaction(&transaction)?)
    } else {
        None
    };
    let simulation_error = simulation
        .as_ref()
        .and_then(|simulation| {
            simulation
                .value
                .err
                .as_ref()
                .map(|error| format!("{error:?}"))
        })
        .or(packet_error);
    let simulation_logs = simulation
        .as_ref()
        .map(|simulation| json!(simulation.value.logs))
        .unwrap_or(Value::Null);
    let simulation_units_consumed = simulation
        .as_ref()
        .and_then(|simulation| simulation.value.units_consumed);

    Ok(PolicyTransactionBuild {
        transaction,
        transaction_packet,
        best_case_single_lookup_table_packet,
        simulation_error,
        simulation_logs,
        simulation_skipped_reason,
        simulation_units_consumed,
    })
}

fn guard_lookup_table_mutations(
    instructions: &[Instruction],
    mode: AltInstructionMode,
    operation_label: &str,
) -> Result<(), Box<dyn Error>> {
    if mode == AltInstructionMode::AllowProvisioning {
        return Ok(());
    }
    for instruction in instructions {
        if let Some(kind) = lookup_table_mutation_kind(instruction) {
            return Err(format!(
                "{operation_label} rejected Address Lookup Table {kind} instruction outside explicit provisioning mode"
            )
            .into());
        }
    }
    Ok(())
}

fn lookup_table_mutation_kind(instruction: &Instruction) -> Option<&'static str> {
    if instruction.program_id != address_lookup_table_program::id() {
        return None;
    }
    match bincode::deserialize::<address_lookup_table_instruction::ProgramInstruction>(
        &instruction.data,
    )
    .ok()?
    {
        address_lookup_table_instruction::ProgramInstruction::CreateLookupTable { .. } => {
            Some("create")
        }
        address_lookup_table_instruction::ProgramInstruction::ExtendLookupTable { .. } => {
            Some("extend")
        }
        _ => None,
    }
}

#[allow(clippy::too_many_arguments)]
fn policy_operation_preview_json(
    operation: &str,
    vault: &SelectedVault,
    settings: Pubkey,
    policy: Pubkey,
    vault_pubkey: Pubkey,
    authority_signer: Pubkey,
    delegated_signer: Pubkey,
    db_delegated_signer_matches: bool,
    universe: &YieldRouteUniverse,
    swap_lanes: &[SwapLane],
    setup: &YieldRouteActionSetup,
    transaction: &PolicyTransactionBuild,
    existing_decoded: Option<&DecodedPolicyAccount>,
) -> Result<Value, Box<dyn Error>> {
    let same_mint_route = setup.same_mint_route()?;
    let jupiter_route = setup.jupiter_route().ok();
    let loyal_hub_route = setup.loyal_hub_route().ok();
    Ok(json!({
        "operation": operation,
        "policyAccount": policy.to_string(),
        "settings": settings.to_string(),
        "vaultIndex": vault.vault_index,
        "vaultPubkey": vault_pubkey.to_string(),
        "authoritySigner": authority_signer.to_string(),
        "delegatedSigner": delegated_signer.to_string(),
        "dbDelegatedSignerMatches": db_delegated_signer_matches,
        "dbDelegatedSigners": vault.delegated_signers.clone(),
        "transaction": policy_transaction_packet_json(transaction),
        "simulationSkippedReason": transaction.simulation_skipped_reason.clone(),
        "constraintCount": setup.spec.constraint_count,
        "instructionCount": setup.spec.instruction_count,
        "stableMints": pubkeys_json(&universe.stable_mints),
        "kaminoMarkets": pubkeys_json(&universe.kamino_markets),
        "kaminoLiquidityMints": pubkeys_json(&universe.kamino_liquidity_mints),
        "templateStableMints": vault.stable_mints.clone(),
        "templateKaminoMarkets": vault.kamino_markets.clone(),
        "templateKaminoLiquidityMints": vault.kamino_liquidity_mints.clone(),
        "swapLanes": swap_lanes_json(swap_lanes),
        "storedSwapLanes": policy_swap_lanes_json(setup, swap_lanes)?,
        "sameMintConstraintIndexes": same_mint_route.instruction_constraint_indexes(),
        "jupiterConstraintIndexes": jupiter_route.as_ref().map(|route| route.instruction_constraint_indexes().to_vec()),
        "loyalHubConstraintIndexes": loyal_hub_route.as_ref().map(|route| route.instruction_constraint_indexes().to_vec()),
        "existingPolicyDecoded": existing_decoded.map(decoded_policy_account_json),
        "simulationError": transaction.simulation_error.clone(),
        "simulationLogs": transaction.simulation_logs.clone(),
        "simulationUnitsConsumed": transaction.simulation_units_consumed,
    }))
}

#[allow(clippy::too_many_arguments)]
fn setup_policy_operation_preview_json(
    operation: &str,
    vault: &SelectedVault,
    settings: Pubkey,
    policy: Pubkey,
    policy_seed: i64,
    vault_pubkey: Pubkey,
    authority_signer: Pubkey,
    delegated_signer: Pubkey,
    db_delegated_signer_matches: bool,
    universe: &YieldRouteUniverse,
    setup: &YieldRouteActionSetup,
    transaction: &PolicyTransactionBuild,
    existing_decoded: Option<&DecodedPolicyAccount>,
) -> Result<Value, Box<dyn Error>> {
    Ok(json!({
        "operation": operation,
        "policyAccount": policy.to_string(),
        "policySeed": policy_seed,
        "settings": settings.to_string(),
        "vaultIndex": vault.vault_index,
        "vaultPubkey": vault_pubkey.to_string(),
        "authoritySigner": authority_signer.to_string(),
        "delegatedSigner": delegated_signer.to_string(),
        "dbDelegatedSignerMatches": db_delegated_signer_matches,
        "dbDelegatedSigners": vault.delegated_signers.clone(),
        "transaction": policy_transaction_packet_json(transaction),
        "simulationSkippedReason": transaction.simulation_skipped_reason.clone(),
        "constraintCount": setup.spec.constraint_count,
        "instructionCount": setup.spec.instruction_count,
        "stableMints": pubkeys_json(&universe.stable_mints),
        "kaminoMarkets": pubkeys_json(&universe.kamino_markets),
        "kaminoLiquidityMints": pubkeys_json(&universe.kamino_liquidity_mints),
        "templateStableMints": vault.stable_mints.clone(),
        "templateKaminoMarkets": vault.kamino_markets.clone(),
        "templateKaminoLiquidityMints": vault.kamino_liquidity_mints.clone(),
        "initObligationConstraintIndex": setup.spec.constraint_count.saturating_sub(1),
        "existingPolicyDecoded": existing_decoded.map(decoded_policy_account_json),
        "simulationError": transaction.simulation_error.clone(),
        "simulationLogs": transaction.simulation_logs.clone(),
        "simulationUnitsConsumed": transaction.simulation_units_consumed,
    }))
}

async fn prepare_durable_route_lookup_table(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    scope: &str,
    payer: Pubkey,
    authority_signer: &dyn Signer,
    required_addresses: &[Pubkey],
    lookup_table_accounts: &mut Vec<AddressLookupTableAccount>,
) -> Result<Value, Box<dyn Error>> {
    let provisioning_enabled =
        options.provision_lookup_table || options.provision_route_lookup_table;
    let cluster = route_lookup_table_cluster(&options.rpc_url);
    let authority = authority_signer.pubkey();
    let mut missing_before =
        missing_lookup_table_addresses(required_addresses, lookup_table_accounts);
    let mut created_lookup_table = None;
    let mut extended_lookup_table = None;
    let mut create_signature = None;
    let mut create_submitted_slot = None;
    let mut create_confirmed_slot = None;
    let mut create_transaction_json = Value::Null;
    let mut extend_transactions = Vec::new();
    let mut warmup = Value::Null;
    let mut provisioning_lock_key = None;
    let mut reusable_lookup_table = reusable_lookup_table_for_missing_addresses(
        rpc,
        authority,
        &missing_before,
        lookup_table_accounts,
    )?;

    if provisioning_enabled && options.execute && !missing_before.is_empty() {
        let provisioning_lock = client
            .acquire_route_lookup_table_provisioning_lock(&cluster, scope, &authority.to_string())
            .await?;
        provisioning_lock_key = Some(provisioning_lock.key().to_owned());

        let table_pubkeys =
            lookup_table_pubkeys_for_scope(client, options, scope, authority).await?;
        *lookup_table_accounts = load_address_lookup_table_accounts(rpc, &table_pubkeys)?;
        missing_before = missing_lookup_table_addresses(required_addresses, lookup_table_accounts);
        reusable_lookup_table = reusable_lookup_table_for_missing_addresses(
            rpc,
            authority,
            &missing_before,
            lookup_table_accounts,
        )?;

        if !missing_before.is_empty() {
            if missing_before.len() > LOOKUP_TABLE_MAX_ADDRESSES {
                return Err(format!(
                    "lookup table needs {} addresses but one table can hold at most {}",
                    missing_before.len(),
                    LOOKUP_TABLE_MAX_ADDRESSES
                )
                .into());
            }

            let lookup_table_address = if let Some(existing) = reusable_lookup_table {
                extended_lookup_table = Some(existing);
                existing
            } else {
                let recent_slot = lookup_table_recent_slot(rpc)?;
                let (create_instruction, lookup_table_address) =
                    address_lookup_table_instruction::create_lookup_table(
                        authority,
                        payer,
                        recent_slot,
                    );
                let create_transaction = build_signed_transaction_for_mode(
                    rpc,
                    payer,
                    &[create_instruction],
                    &[],
                    &[authority_signer],
                    "route lookup table create",
                    None,
                    AltInstructionMode::AllowProvisioning,
                )?;
                create_transaction_json = policy_transaction_json(&create_transaction);
                if let Some(error) = create_transaction.simulation_error.as_ref() {
                    return Err(format!("lookup table create simulation failed: {error}").into());
                }
                let submitted_slot = rpc.get_slot()?;
                let signature = rpc
                    .send_and_confirm_transaction(&create_transaction.transaction)?
                    .to_string();
                let confirmed_slot = rpc.get_slot()?;
                created_lookup_table = Some(lookup_table_address);
                create_signature = Some(signature);
                create_submitted_slot = Some(i64::try_from(submitted_slot)?);
                create_confirmed_slot = Some(i64::try_from(confirmed_slot)?);
                lookup_table_address
            };

            for chunk in missing_before.chunks(LOOKUP_TABLE_EXTEND_CHUNK_SIZE) {
                let extend_instruction = address_lookup_table_instruction::extend_lookup_table(
                    lookup_table_address,
                    authority,
                    Some(payer),
                    chunk.to_vec(),
                );
                let extend_transaction = build_signed_transaction_for_mode(
                    rpc,
                    payer,
                    &[extend_instruction],
                    &[],
                    &[authority_signer],
                    "route lookup table extend",
                    None,
                    AltInstructionMode::AllowProvisioning,
                )?;
                let transaction_json = policy_transaction_json(&extend_transaction);
                if let Some(error) = extend_transaction.simulation_error.as_ref() {
                    return Err(format!("lookup table extend simulation failed: {error}").into());
                }
                let submitted_slot = rpc.get_slot()?;
                let signature = rpc
                    .send_and_confirm_transaction(&extend_transaction.transaction)?
                    .to_string();
                let confirmed_slot = rpc.get_slot()?;
                extend_transactions.push(json!({
                    "signature": signature,
                    "submittedSlot": i64::try_from(submitted_slot)?,
                    "confirmedSlot": i64::try_from(confirmed_slot)?,
                    "addressCount": chunk.len(),
                    "addresses": pubkeys_json(chunk),
                    "transaction": transaction_json,
                }));
            }

            warmup = wait_for_lookup_table_warmup(rpc, lookup_table_address)?;
            let mut table_pubkeys =
                lookup_table_pubkeys_for_scope(client, options, scope, authority).await?;
            table_pubkeys.push(lookup_table_address);
            table_pubkeys.sort();
            table_pubkeys.dedup();
            *lookup_table_accounts = load_address_lookup_table_accounts(rpc, &table_pubkeys)?;
            let recorded_account = lookup_table_accounts
                .iter()
                .find(|account| account.key == lookup_table_address)
                .ok_or("provisioned lookup table was not reloadable")?;
            let last_extended_slot = lookup_table_last_extended_slot(rpc, lookup_table_address)?;
            let extend_signatures_json = json!(extend_transactions
                .iter()
                .filter_map(|value| value.get("signature").and_then(Value::as_str))
                .collect::<Vec<_>>());
            record_durable_lookup_table(
                client,
                options,
                scope,
                lookup_table_address,
                authority,
                payer,
                &recorded_account.addresses,
                create_signature.clone(),
                extend_signatures_json,
                last_extended_slot.map(i64::try_from).transpose()?,
                lookup_table_warmup_slot(&warmup),
                "usable",
                Some("route lookup table provisioning".to_owned()),
            )
            .await?;
        }
        provisioning_lock.release().await?;
    }

    let missing_after = missing_lookup_table_addresses(required_addresses, lookup_table_accounts);
    let status = if missing_after.is_empty() {
        "lookup_table_coverage_ready"
    } else if !provisioning_enabled {
        "lookup_table_coverage_missing"
    } else if options.execute {
        "lookup_table_coverage_missing"
    } else if reusable_lookup_table.is_some() {
        "lookup_table_would_extend"
    } else {
        "lookup_table_would_create"
    };

    Ok(json!({
        "enabled": provisioning_enabled,
        "mode": "route_provisioning",
        "execute": options.execute,
        "status": status,
        "cluster": cluster,
        "scope": scope,
        "authority": authority.to_string(),
        "payer": payer.to_string(),
        "provisioningLock": provisioning_lock_key,
        "requiredAddresses": pubkeys_json(required_addresses),
        "requiredAddressCount": required_addresses.len(),
        "missingBeforeProvision": pubkeys_json(&missing_before),
        "missingBeforeProvisionCount": missing_before.len(),
        "wouldCreateLookupTable": provisioning_enabled && !options.execute && !missing_before.is_empty() && reusable_lookup_table.is_none(),
        "wouldExtendLookupTable": provisioning_enabled && !options.execute && !missing_before.is_empty() && reusable_lookup_table.is_some(),
        "reusableLookupTable": reusable_lookup_table.map(|pubkey| pubkey.to_string()),
        "extendedLookupTable": extended_lookup_table.map(|pubkey| pubkey.to_string()),
        "createdLookupTable": created_lookup_table.map(|pubkey| pubkey.to_string()),
        "createSignature": create_signature,
        "createSubmittedSlot": create_submitted_slot,
        "createConfirmedSlot": create_confirmed_slot,
        "createTransaction": create_transaction_json,
        "extendTransactions": extend_transactions,
        "warmup": warmup,
        "coverageAfterProvision": lookup_table_coverage_json(required_addresses, lookup_table_accounts),
    }))
}

fn reusable_lookup_table_for_missing_addresses(
    rpc: &RpcClient,
    authority: Pubkey,
    missing_addresses: &[Pubkey],
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> Result<Option<Pubkey>, Box<dyn Error>> {
    if missing_addresses.is_empty() {
        return Ok(None);
    }
    for account in lookup_table_accounts {
        if account.addresses.len() + missing_addresses.len() > LOOKUP_TABLE_MAX_ADDRESSES {
            continue;
        }
        let raw = rpc.get_account(&account.key)?;
        let table = AddressLookupTable::deserialize(&raw.data).map_err(|error| {
            format!(
                "failed to deserialize address lookup table {} for authority check: {error:?}",
                account.key
            )
        })?;
        if table.meta.authority == Some(authority) {
            return Ok(Some(account.key));
        }
    }
    Ok(None)
}

fn lookup_table_last_extended_slot(
    rpc: &RpcClient,
    lookup_table_address: Pubkey,
) -> Result<Option<u64>, Box<dyn Error>> {
    let account = rpc.get_account(&lookup_table_address)?;
    let table = AddressLookupTable::deserialize(&account.data).map_err(|error| {
        format!("failed to deserialize address lookup table {lookup_table_address}: {error:?}")
    })?;
    Ok(Some(table.meta.last_extended_slot))
}

fn lookup_table_recent_slot(rpc: &RpcClient) -> Result<u64, Box<dyn Error>> {
    Ok(rpc.get_slot_with_commitment(CommitmentConfig::finalized())?)
}

fn wait_for_lookup_table_warmup(
    rpc: &RpcClient,
    lookup_table_address: Pubkey,
) -> Result<Value, Box<dyn Error>> {
    let mut last_extended_slot = None;
    for _ in 0..LOOKUP_TABLE_WARMUP_MAX_POLLS {
        let account = rpc.get_account(&lookup_table_address)?;
        let table = AddressLookupTable::deserialize(&account.data).map_err(|error| {
            format!("failed to deserialize address lookup table {lookup_table_address}: {error:?}")
        })?;
        let current_slot = rpc.get_slot()?;
        last_extended_slot = Some(table.meta.last_extended_slot);
        if current_slot > table.meta.last_extended_slot {
            return Ok(json!({
                "lookupTable": lookup_table_address.to_string(),
                "lastExtendedSlot": i64::try_from(table.meta.last_extended_slot)?,
                "readySlot": i64::try_from(current_slot)?,
                "ready": true,
            }));
        }
        thread::sleep(Duration::from_millis(LOOKUP_TABLE_WARMUP_POLL_MS));
    }
    Err(format!(
        "lookup table {lookup_table_address} did not warm up after {} polls; last_extended_slot={:?}",
        LOOKUP_TABLE_WARMUP_MAX_POLLS, last_extended_slot
    )
    .into())
}

fn lookup_table_warmup_slot(warmup: &Value) -> Option<i64> {
    warmup
        .get("readySlot")
        .and_then(Value::as_i64)
        .or_else(|| warmup.get("usableSlot").and_then(Value::as_i64))
        .or_else(|| warmup.get("currentSlot").and_then(Value::as_i64))
}

fn missing_lookup_table_addresses(
    required_addresses: &[Pubkey],
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> Vec<Pubkey> {
    let present = lookup_table_accounts
        .iter()
        .flat_map(|account| account.addresses.iter().copied())
        .collect::<BTreeSet<_>>();
    required_addresses
        .iter()
        .copied()
        .filter(|address| !present.contains(address))
        .collect()
}

fn lookup_table_coverage_json(
    required_addresses: &[Pubkey],
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> Value {
    let missing = missing_lookup_table_addresses(required_addresses, lookup_table_accounts);
    json!({
        "coversAllRequiredAddresses": missing.is_empty(),
        "missingAddresses": pubkeys_json(&missing),
        "missingAddressCount": missing.len(),
        "lookupTables": lookup_table_accounts.iter().map(|account| {
            json!({
                "account": account.key.to_string(),
                "addressCount": account.addresses.len(),
                "coveredRequiredAddresses": pubkeys_json(
                    &required_addresses
                        .iter()
                        .copied()
                        .filter(|address| account.addresses.contains(address))
                        .collect::<Vec<_>>()
                ),
            })
        }).collect::<Vec<_>>(),
    })
}

async fn route_lookup_table_reuse_coverage(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    options: &CliOptions,
    scope: &str,
    fee_payer: Pubkey,
    delegated_signer: Pubkey,
    route_execution: &RouteExecutionPlan,
) -> Result<RouteLookupTableCoverage, Box<dyn Error>> {
    let lookup_table_pubkeys =
        lookup_table_pubkeys_for_scope(client, options, scope, fee_payer).await?;
    let lookup_table_accounts = load_address_lookup_table_accounts(rpc, &lookup_table_pubkeys)?;
    let mut transaction_instructions = route_execution.pre_instructions.clone();
    transaction_instructions.extend(route_execution.instructions.iter().cloned());
    let signer_pubkeys = same_mint_route_signer_pubkeys(fee_payer, delegated_signer);
    let required_addresses =
        best_case_lookup_table_addresses(fee_payer, &transaction_instructions, &signer_pubkeys);
    let missing_addresses =
        missing_lookup_table_addresses(&required_addresses, &lookup_table_accounts);

    Ok(RouteLookupTableCoverage {
        scope: scope.to_owned(),
        lookup_table_accounts,
        required_addresses,
        missing_addresses,
    })
}

fn ensure_route_lookup_table_coverage(
    scope: &str,
    missing_lookup_addresses: &[Pubkey],
) -> Result<(), Box<dyn Error>> {
    if missing_lookup_addresses.is_empty() {
        return Ok(());
    }
    Err(format!(
        "lookup_table_coverage_missing: route scope {} is missing {} required address(es): {}",
        scope,
        missing_lookup_addresses.len(),
        pubkeys_json(missing_lookup_addresses).join(", ")
    )
    .into())
}

fn lookup_table_pubkeys_from_options(options: &CliOptions) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let mut pubkeys = options.lookup_tables.clone();
    if let Ok(raw) = env::var("YIELD_ROUTE_LOOKUP_TABLES") {
        pubkeys.extend(parse_lookup_table_list(&raw)?);
    }
    pubkeys.sort();
    pubkeys.dedup();
    Ok(pubkeys)
}

async fn lookup_table_pubkeys_for_scope(
    client: &NeonSqlClient,
    options: &CliOptions,
    scope: &str,
    authority: Pubkey,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let mut pubkeys = lookup_table_pubkeys_from_options(options)?;
    let cluster = route_lookup_table_cluster(&options.rpc_url);
    let records = client
        .durable_route_lookup_tables(&cluster, scope, &authority.to_string())
        .await?;
    for record in records {
        pubkeys.push(Pubkey::from_str(&record.table_address).map_err(|error| {
            format!(
                "registered lookup table {} for scope {scope} is not a public key: {error}",
                record.table_address
            )
        })?);
    }
    pubkeys.sort();
    pubkeys.dedup();
    Ok(pubkeys)
}

fn route_lookup_table_cluster(rpc_url: &str) -> String {
    if rpc_url.contains("devnet") {
        "devnet".to_owned()
    } else if rpc_url.contains("testnet") {
        "testnet".to_owned()
    } else if rpc_url.contains("localhost") || rpc_url.contains("127.0.0.1") {
        "localnet".to_owned()
    } else {
        "mainnet-beta".to_owned()
    }
}

fn same_mint_route_lookup_table_scope(vault: &SelectedVault, reserve_move: &ReserveMove) -> String {
    same_mint_route_lookup_table_scope_for_reserves(
        vault,
        &reserve_move.source_reserve,
        &reserve_move.target_reserve,
    )
}

fn same_mint_route_lookup_table_scope_for_reserves(
    vault: &SelectedVault,
    source_reserve: &str,
    target_reserve: &str,
) -> String {
    format!(
        "same_mint_kamino:{}:{}:{}:{}",
        vault.settings, vault.vault_index, source_reserve, target_reserve
    )
}

fn route_lookup_table_address_hash(addresses: &[Pubkey]) -> String {
    let mut ordered = addresses
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    ordered.sort();
    let mut hasher = Sha256::new();
    for address in ordered {
        hasher.update(address.as_bytes());
        hasher.update([0]);
    }
    format!("{:x}", hasher.finalize())
}

fn lookup_table_addresses_json(addresses: &[Pubkey]) -> Value {
    json!(addresses
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>())
}

async fn record_durable_lookup_table(
    client: &NeonSqlClient,
    options: &CliOptions,
    scope: &str,
    table_address: Pubkey,
    authority: Pubkey,
    payer: Pubkey,
    addresses: &[Pubkey],
    create_signature: Option<String>,
    extend_signatures: Value,
    last_extended_slot: Option<i64>,
    warmup_slot: Option<i64>,
    status: &str,
    notes: Option<String>,
) -> Result<(), Box<dyn Error>> {
    client
        .upsert_route_lookup_table(RouteLookupTableUpsert {
            cluster: route_lookup_table_cluster(&options.rpc_url),
            scope: scope.to_owned(),
            table_address: table_address.to_string(),
            authority: authority.to_string(),
            payer: payer.to_string(),
            status: status.to_owned(),
            durable: true,
            address_count: i32::try_from(addresses.len())?,
            address_hash: route_lookup_table_address_hash(addresses),
            addresses: lookup_table_addresses_json(addresses),
            create_signature,
            extend_signatures,
            last_extended_slot,
            warmup_slot,
            notes,
        })
        .await?;
    Ok(())
}

fn parse_lookup_table_list(raw: &str) -> Result<Vec<Pubkey>, String> {
    raw.split(|character: char| character == ',' || character.is_ascii_whitespace())
        .filter(|value| !value.trim().is_empty())
        .map(|value| {
            let value = value.trim();
            Pubkey::from_str(value)
                .map_err(|_| format!("lookup table address {value:?} is not a public key"))
        })
        .collect()
}

fn load_address_lookup_table_accounts(
    rpc: &RpcClient,
    table_addresses: &[Pubkey],
) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
    table_addresses
        .iter()
        .map(|table_address| {
            let account = rpc.get_account(table_address)?;
            let table = AddressLookupTable::deserialize(&account.data).map_err(|error| {
                format!("failed to deserialize address lookup table {table_address}: {error:?}")
            })?;
            Ok(AddressLookupTableAccount {
                key: *table_address,
                addresses: table.addresses.to_vec(),
            })
        })
        .collect()
}

fn compile_versioned_transaction(
    payer: Pubkey,
    instructions: &[Instruction],
    lookup_table_accounts: &[AddressLookupTableAccount],
    blockhash: Hash,
    signers: &[&dyn Signer],
) -> Result<VersionedTransaction, Box<dyn Error>> {
    let message = v0::Message::try_compile(&payer, instructions, lookup_table_accounts, blockhash)?;
    Ok(VersionedTransaction::try_new(
        VersionedMessage::V0(message),
        signers,
    )?)
}

fn best_case_single_lookup_table_packet_summary(
    payer: Pubkey,
    instructions: &[Instruction],
    blockhash: Hash,
    signers: &[&dyn Signer],
) -> Result<Option<TransactionPacketSummary>, Box<dyn Error>> {
    let signer_pubkeys = signers
        .iter()
        .map(|signer| signer.pubkey())
        .collect::<Vec<_>>();
    let lookup_addresses = best_case_lookup_table_addresses(payer, instructions, &signer_pubkeys);
    if lookup_addresses.is_empty() {
        return Ok(None);
    }
    let lookup_table_accounts = vec![AddressLookupTableAccount {
        key: Pubkey::new_from_array([42; 32]),
        addresses: lookup_addresses,
    }];
    let transaction = compile_versioned_transaction(
        payer,
        instructions,
        &lookup_table_accounts,
        blockhash,
        signers,
    )?;
    let mut summary = transaction_packet_summary(&transaction, &lookup_table_accounts)?;
    for (summary, account) in summary
        .lookup_table_accounts
        .iter_mut()
        .zip(lookup_table_accounts.iter())
    {
        summary.addresses = Some(pubkeys_json(&account.addresses));
    }
    Ok(Some(summary))
}

fn best_case_lookup_table_addresses(
    payer: Pubkey,
    instructions: &[Instruction],
    signer_pubkeys: &[Pubkey],
) -> Vec<Pubkey> {
    let mut static_required = vec![payer];
    static_required.extend_from_slice(signer_pubkeys);
    static_required.sort();
    static_required.dedup();

    let mut addresses = Vec::new();
    for instruction in instructions {
        for account in &instruction.accounts {
            if !account.is_signer {
                push_lookup_candidate(&mut addresses, &static_required, account.pubkey);
            }
        }
    }
    addresses.sort();
    addresses.dedup();
    addresses
}

fn push_lookup_candidate(addresses: &mut Vec<Pubkey>, static_required: &[Pubkey], pubkey: Pubkey) {
    if !static_required.binary_search(&pubkey).is_ok() {
        addresses.push(pubkey);
    }
}

fn transaction_packet_summary(
    transaction: &VersionedTransaction,
    lookup_table_accounts: &[AddressLookupTableAccount],
) -> Result<TransactionPacketSummary, Box<dyn Error>> {
    let packet_size_bytes = bincode::serialize(transaction)?.len();
    let VersionedMessage::V0(message) = &transaction.message else {
        return Err("expected v0 transaction message".into());
    };
    let signer_count = usize::from(message.header.num_required_signatures);
    Ok(TransactionPacketSummary {
        version: "v0",
        fee_payer: message
            .account_keys
            .first()
            .map(ToString::to_string)
            .unwrap_or_default(),
        signer_pubkeys: message
            .account_keys
            .iter()
            .take(signer_count)
            .map(ToString::to_string)
            .collect(),
        packet_size_bytes,
        packet_data_size_bytes: PACKET_DATA_SIZE,
        fits_packet_data_size: packet_size_bytes <= PACKET_DATA_SIZE,
        static_account_key_count: message.account_keys.len(),
        address_table_lookup_count: message.address_table_lookups.len(),
        loaded_writable_address_count: message
            .address_table_lookups
            .iter()
            .map(|lookup| lookup.writable_indexes.len())
            .sum(),
        loaded_readonly_address_count: message
            .address_table_lookups
            .iter()
            .map(|lookup| lookup.readonly_indexes.len())
            .sum(),
        compiled_instruction_count: message.instructions.len(),
        instruction_data_bytes: message
            .instructions
            .iter()
            .map(|instruction| instruction.data.len())
            .sum(),
        lookup_table_accounts: lookup_table_accounts
            .iter()
            .map(|account| LookupTableAccountSummary {
                account: account.key.to_string(),
                address_count: account.addresses.len(),
                addresses: None,
            })
            .collect(),
    })
}

fn policy_transaction_packet_json(transaction: &PolicyTransactionBuild) -> Value {
    let mut value = transaction_packet_json(&transaction.transaction_packet);
    if let Value::Object(ref mut object) = value {
        object.insert(
            "bestCaseSingleLookupTable".to_owned(),
            transaction
                .best_case_single_lookup_table_packet
                .as_ref()
                .map(transaction_packet_json)
                .unwrap_or(Value::Null),
        );
    }
    value
}

fn transaction_packet_json(summary: &TransactionPacketSummary) -> Value {
    json!({
        "version": summary.version,
        "feePayer": summary.fee_payer,
        "signerPubkeys": summary.signer_pubkeys,
        "packetSizeBytes": summary.packet_size_bytes,
        "packetDataSizeBytes": summary.packet_data_size_bytes,
        "fitsPacketDataSize": summary.fits_packet_data_size,
        "lookupTableCount": summary.lookup_table_accounts.len(),
        "lookupTableAddressCount": summary.lookup_table_accounts.iter().map(|account| account.address_count).sum::<usize>(),
        "staticAccountKeyCount": summary.static_account_key_count,
        "addressTableLookupCount": summary.address_table_lookup_count,
        "loadedWritableAddressCount": summary.loaded_writable_address_count,
        "loadedReadonlyAddressCount": summary.loaded_readonly_address_count,
        "compiledInstructionCount": summary.compiled_instruction_count,
        "instructionDataBytes": summary.instruction_data_bytes,
        "instructionDataExceedsPacketLimit": summary.instruction_data_bytes > summary.packet_data_size_bytes,
        "lookupTables": summary.lookup_table_accounts.iter().map(|account| {
            let mut value = json!({
                "account": account.account,
                "addressCount": account.address_count,
            });
            if let (Value::Object(object), Some(addresses)) = (&mut value, &account.addresses) {
                object.insert("addresses".to_owned(), json!(addresses));
            }
            value
        }).collect::<Vec<_>>(),
    })
}

fn policy_transaction_json(transaction: &PolicyTransactionBuild) -> Value {
    let obligation_stale = policy_transaction_has_klend_obligation_stale(transaction);
    json!({
        "transaction": policy_transaction_packet_json(transaction),
        "simulationError": transaction.simulation_error,
        "simulationSkippedReason": transaction.simulation_skipped_reason,
        "simulationUnitsConsumed": transaction.simulation_units_consumed,
        "simulationLogs": transaction.simulation_logs,
        "klendObligationStale": obligation_stale,
        "requiresRefreshObligationPolicy": false,
        "refreshObligationPolicyNote": obligation_stale.then_some(
            "KLend deposit/withdraw needs a fresh obligation; the script now emits refresh_obligation as a public pre-instruction before protected value movement"
        ),
    })
}

fn policy_transaction_has_klend_obligation_stale(transaction: &PolicyTransactionBuild) -> bool {
    simulation_indicates_klend_obligation_stale(
        transaction.simulation_error.as_deref(),
        &transaction.simulation_logs,
    )
}

fn simulation_indicates_klend_obligation_stale(
    simulation_error: Option<&str>,
    simulation_logs: &Value,
) -> bool {
    simulation_error.is_some_and(|error| error.contains("Custom(6017)") || error.contains("0x1781"))
        || json_logs_contain(simulation_logs, "ObligationStale")
        || json_logs_contain(simulation_logs, "Obligation is stale and must be refreshed")
}

fn json_logs_contain(value: &Value, needle: &str) -> bool {
    match value {
        Value::Array(items) => items
            .iter()
            .any(|item| item.as_str().is_some_and(|log| log.contains(needle))),
        Value::String(log) => log.contains(needle),
        _ => false,
    }
}

async fn load_position_summaries(
    client: &NeonSqlClient,
    vault_id: VaultId,
) -> Result<Vec<PositionSummary>, Box<dyn Error>> {
    let current_positions = client.current_positions(vault_id).await?;
    Ok(current_positions
        .into_iter()
        .map(|position| PositionSummary {
            reserve: position.reserve,
            liquidity_mint: position.liquidity_mint,
            amount_raw: position.amount_raw,
            has_value: position.has_value,
            snapshot_id: position.snapshot_id,
            supply_apy_bps: position.supply_apy_bps,
            planning_metadata: position.planning_metadata,
        })
        .collect())
}

async fn load_prepared_same_mint_decision(
    pool: &PgPool,
    decision_id: DecisionId,
) -> Result<PreparedSameMintDecision, Box<dyn Error>> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            id,
            vault_id,
            source_snapshot_id,
            status::text AS status,
            source_reserve,
            target_reserve,
            liquidity_mint,
            source_liquidity_mint,
            target_liquidity_mint,
            amount_raw,
            source_apy_bps,
            target_apy_bps,
            estimated_edge_bps,
            estimated_cost_lamports,
            execution_plan,
            idempotency_key
        FROM loyal_yield.rebalance_decisions
        WHERE id = $1
        "#,
    )
    .bind(decision_id.as_i64())
    .fetch_one(pool)
    .await?;

    let status: String = row.try_get("status")?;
    if DecisionStatus::parse(&status) != Some(DecisionStatus::Planned) {
        return Err(format!(
            "decision {} is {}, expected planned before execution",
            decision_id.as_i64(),
            status
        )
        .into());
    }
    let execution_plan: Value = row.try_get("execution_plan")?;
    let kind = execution_plan
        .get("kind")
        .and_then(Value::as_str)
        .unwrap_or_default();
    if kind != "same_mint" {
        return Err(format!(
            "decision {} execution_plan.kind is {kind:?}, expected same_mint",
            decision_id.as_i64()
        )
        .into());
    }

    let decision = PreparedSameMintDecision {
        id: DecisionId(row.try_get("id")?),
        vault_id: VaultId(row.try_get("vault_id")?),
        source_snapshot_id: SnapshotId(required_i64_column(&row, "source_snapshot_id")?),
        source_reserve: required_string_column(&row, "source_reserve")?,
        target_reserve: required_string_column(&row, "target_reserve")?,
        liquidity_mint: required_string_column(&row, "liquidity_mint")?,
        source_liquidity_mint: required_string_column(&row, "source_liquidity_mint")?,
        target_liquidity_mint: required_string_column(&row, "target_liquidity_mint")?,
        amount_raw: required_i64_column(&row, "amount_raw")?,
        source_apy_bps: required_i64_column(&row, "source_apy_bps")?,
        target_apy_bps: required_i64_column(&row, "target_apy_bps")?,
        estimated_edge_bps: required_i64_column(&row, "estimated_edge_bps")?,
        estimated_cost_lamports: row.try_get("estimated_cost_lamports")?,
        execution_plan,
        idempotency_key: row.try_get("idempotency_key")?,
    };
    validate_prepared_decision_plan_fields(&decision)?;
    Ok(decision)
}

fn required_string_column(
    row: &loyal_yield_orchestrator::sqlx::postgres::PgRow,
    column: &'static str,
) -> Result<String, Box<dyn Error>> {
    row.try_get::<Option<String>, _>(column)?
        .ok_or_else(|| format!("prepared same-mint decision is missing {column}").into())
}

fn required_i64_column(
    row: &loyal_yield_orchestrator::sqlx::postgres::PgRow,
    column: &'static str,
) -> Result<i64, Box<dyn Error>> {
    row.try_get::<Option<i64>, _>(column)?
        .ok_or_else(|| format!("prepared same-mint decision is missing {column}").into())
}

fn validate_prepared_decision_plan_fields(
    decision: &PreparedSameMintDecision,
) -> Result<(), Box<dyn Error>> {
    require_plan_string(decision, "source_reserve", &decision.source_reserve)?;
    require_plan_string(decision, "target_reserve", &decision.target_reserve)?;
    require_plan_string(decision, "liquidity_mint", &decision.liquidity_mint)?;
    require_optional_plan_string(
        decision,
        "source_liquidity_mint",
        &decision.source_liquidity_mint,
    )?;
    require_optional_plan_string(
        decision,
        "target_liquidity_mint",
        &decision.target_liquidity_mint,
    )?;
    if decision.source_liquidity_mint != decision.liquidity_mint {
        return Err(format!(
            "decision {} source_liquidity_mint {} does not match liquidity_mint {}",
            decision.id, decision.source_liquidity_mint, decision.liquidity_mint
        )
        .into());
    }
    if decision.target_liquidity_mint != decision.liquidity_mint {
        return Err(format!(
            "decision {} target_liquidity_mint {} does not match liquidity_mint {}",
            decision.id, decision.target_liquidity_mint, decision.liquidity_mint
        )
        .into());
    }
    require_plan_i64(decision, "amount_raw", decision.amount_raw)?;
    require_plan_string(
        decision,
        "route_amount_semantics",
        ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
    )?;
    require_plan_i64(
        decision,
        "redeemable_source_liquidity_amount_raw",
        decision.amount_raw,
    )?;
    if decision.source_snapshot_id.as_i64() <= 0 {
        return Err(format!(
            "decision {} source_snapshot_id {} is not a persisted snapshot",
            decision.id,
            decision.source_snapshot_id.as_i64()
        )
        .into());
    }
    if decision.idempotency_key.trim().is_empty() {
        return Err(format!("decision {} idempotency_key is empty", decision.id).into());
    }
    Ok(())
}

fn require_plan_string(
    decision: &PreparedSameMintDecision,
    field: &'static str,
    expected: &str,
) -> Result<(), Box<dyn Error>> {
    let actual = decision
        .execution_plan
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(|| format!("decision {} execution_plan.{field} is missing", decision.id))?;
    if actual != expected {
        return Err(format!(
            "decision {} execution_plan.{field} {actual} does not match row value {expected}",
            decision.id
        )
        .into());
    }
    Ok(())
}

fn require_optional_plan_string(
    decision: &PreparedSameMintDecision,
    field: &'static str,
    expected: &str,
) -> Result<(), Box<dyn Error>> {
    let Some(actual) = decision.execution_plan.get(field).and_then(Value::as_str) else {
        return Ok(());
    };
    if actual != expected {
        return Err(format!(
            "decision {} execution_plan.{field} {actual} does not match row value {expected}",
            decision.id
        )
        .into());
    }
    Ok(())
}

fn require_plan_i64(
    decision: &PreparedSameMintDecision,
    field: &'static str,
    expected: i64,
) -> Result<(), Box<dyn Error>> {
    let actual = decision
        .execution_plan
        .get(field)
        .and_then(Value::as_i64)
        .ok_or_else(|| format!("decision {} execution_plan.{field} is missing", decision.id))?;
    if actual != expected {
        return Err(format!(
            "decision {} execution_plan.{field} {actual} does not match row value {expected}",
            decision.id
        )
        .into());
    }
    Ok(())
}

fn plan_i64(plan: &Value, field: &'static str) -> Option<i64> {
    let value = plan.get(field)?;
    value
        .as_i64()
        .or_else(|| value.as_u64().and_then(|amount| i64::try_from(amount).ok()))
        .or_else(|| value.as_str().and_then(|amount| amount.parse::<i64>().ok()))
}

fn validate_execution_decision_route(
    decision: &PreparedSameMintDecision,
    reserve_move: &ReserveMove,
) -> Result<(), Box<dyn Error>> {
    if decision.source_reserve != reserve_move.source_reserve {
        return Err(format!(
            "persisted decision source reserve {} does not match requested source reserve {}",
            decision.source_reserve, reserve_move.source_reserve
        )
        .into());
    }
    if decision.target_reserve != reserve_move.target_reserve {
        return Err(format!(
            "persisted decision target reserve {} does not match requested target reserve {}",
            decision.target_reserve, reserve_move.target_reserve
        )
        .into());
    }
    Ok(())
}

fn same_mint_input_from_decision(decision: &PreparedSameMintDecision) -> SameMintRebalanceInput {
    SameMintRebalanceInput {
        vault_id: Some(decision.vault_id),
        settings: None,
        vault_index: None,
        source_reserve: decision.source_reserve.clone(),
        target_reserve: decision.target_reserve.clone(),
        liquidity_mint: decision.liquidity_mint.clone(),
        amount_raw: decision.amount_raw,
        route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
        source_amount_semantics: decision
            .execution_plan
            .get("source_amount_semantics")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned),
        source_collateral_amount_raw: plan_i64(
            &decision.execution_plan,
            "source_collateral_amount_raw",
        ),
        redeemable_source_liquidity_amount_raw: plan_i64(
            &decision.execution_plan,
            "redeemable_source_liquidity_amount_raw",
        ),
        idle_vault_liquidity_amount_raw: plan_i64(
            &decision.execution_plan,
            "idle_vault_liquidity_amount_raw",
        ),
        expected_source_snapshot_id: decision.source_snapshot_id,
        source_apy_bps: decision.source_apy_bps,
        target_apy_bps: decision.target_apy_bps,
        estimated_edge_bps: decision.estimated_edge_bps,
        estimated_cost_lamports: decision.estimated_cost_lamports,
        dry_run: false,
    }
}

async fn load_user_position_seed_preview(
    pool: &PgPool,
    vault: &SelectedVault,
    reserve_move: &ReserveMove,
    chain_preview: Option<&ChainReconcilePreview>,
    direction: Direction,
) -> Result<Option<UserPositionSeedPreview>, Box<dyn Error>> {
    let rows = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            id,
            current_reserve,
            current_market,
            current_liquidity_mint,
            current_amount_raw,
            current_observed_slot,
            current_observed_at
        FROM loyal_yield.user_yield_positions
        WHERE settings = $1
          AND vault_index = $2
          AND vault_pubkey = $3
          AND status::text = 'active'
        ORDER BY current_observed_at DESC NULLS LAST, id DESC
        "#,
    )
    .bind(&vault.settings)
    .bind(vault.vault_index)
    .bind(&vault.vault_pubkey)
    .fetch_all(pool)
    .await?;

    if rows.is_empty() {
        return Ok(None);
    }

    let rows = rows
        .into_iter()
        .map(|row| {
            Ok(UserPositionSeedRow {
                id: row.try_get("id")?,
                current_reserve: row.try_get("current_reserve")?,
                current_market: row.try_get("current_market")?,
                current_liquidity_mint: row.try_get("current_liquidity_mint")?,
                current_amount_raw: row.try_get("current_amount_raw")?,
                current_observed_slot: row.try_get("current_observed_slot")?,
                current_observed_at: row.try_get("current_observed_at")?,
            })
        })
        .collect::<Result<Vec<_>, loyal_yield_orchestrator::sqlx::Error>>()?;

    let source_reserve = reserve_move.source_reserve.clone();
    let target_reserve = reserve_move.target_reserve.clone();
    let source_row = rows
        .iter()
        .find(|row| row.current_reserve == source_reserve);
    if source_row.is_none() {
        return Ok(Some(UserPositionSeedPreview {
            source: "user_yield_positions".to_owned(),
            rows,
            positions: Vec::new(),
        }));
    }
    let source_row = source_row.expect("checked some");
    let expected_source_market = chain_preview
        .and_then(|preview| chain_position_for_reserve(preview, &reserve_move.source_reserve).ok())
        .map(|position| position.market.clone())
        .or_else(|| {
            if reserve_move.source_reserve == direction.source_reserve() {
                Some(direction.source_market().to_owned())
            } else {
                None
            }
        });
    if let Some(expected_source_market) = expected_source_market {
        if source_row.current_market != expected_source_market {
            return Err(format!(
                "user_yield_positions row {} has market {}, expected {} for reserve {}",
                source_row.id,
                source_row.current_market,
                expected_source_market,
                source_row.current_reserve
            )
            .into());
        }
    }

    let target_amount = rows
        .iter()
        .find(|row| {
            row.current_reserve == target_reserve
                && row.current_liquidity_mint == source_row.current_liquidity_mint
        })
        .map(|row| row.current_amount_raw)
        .unwrap_or_default();
    let liquidity_mint = source_row.current_liquidity_mint.clone();
    let positions = vec![
        PositionSummary {
            reserve: source_reserve,
            liquidity_mint: liquidity_mint.clone(),
            amount_raw: source_row.current_amount_raw,
            has_value: source_row.current_amount_raw > 0,
            snapshot_id: SnapshotId(0),
            supply_apy_bps: None,
            planning_metadata: json!({
                "source": "user_yield_positions",
                "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                "redeemable_source_liquidity_amount_raw": source_row.current_amount_raw.to_string(),
            }),
        },
        PositionSummary {
            reserve: target_reserve,
            liquidity_mint,
            amount_raw: target_amount,
            has_value: target_amount > 0,
            snapshot_id: SnapshotId(0),
            supply_apy_bps: None,
            planning_metadata: json!({
                "source": "user_yield_positions",
                "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                "redeemable_source_liquidity_amount_raw": target_amount.to_string(),
            }),
        },
    ];

    Ok(Some(UserPositionSeedPreview {
        source: "user_yield_positions".to_owned(),
        rows,
        positions,
    }))
}

fn user_position_seed_reconciled_state(
    seed: &UserPositionSeedPreview,
    reserve_move: &ReserveMove,
    target_market: &str,
) -> Result<ReconciledVaultState, Box<dyn Error>> {
    let source_reserve = reserve_move.source_reserve.clone();
    let target_reserve = reserve_move.target_reserve.clone();
    let source_row = seed
        .rows
        .iter()
        .find(|row| row.current_reserve == source_reserve)
        .ok_or_else(|| {
            format!("user_yield_positions seed has no active source reserve {source_reserve}")
        })?;
    let source_amount = amount_i64_to_u64(source_row.current_amount_raw, "source amount")?;

    let target_row = seed.rows.iter().find(|row| {
        row.current_reserve == target_reserve
            && row.current_liquidity_mint == source_row.current_liquidity_mint
    });
    let target_amount = target_row
        .map(|row| amount_i64_to_u64(row.current_amount_raw, "target amount"))
        .transpose()?
        .unwrap_or_default();

    Ok(ReconciledVaultState {
        observed_slot: source_row.current_observed_slot,
        observed_at: source_row.current_observed_at,
        chain_slot: Some(source_row.current_observed_slot),
        lock_attempt_id: None,
        context: json!({
            "kind": "same_mint_user_position_seed",
            "source": seed.source,
            "source_position_id": source_row.id,
            "source_reserve": source_row.current_reserve,
            "target_reserve": target_reserve,
            "amount_raw": source_row.current_amount_raw.to_string(),
        }),
        positions: vec![
            ReconciledReservePosition {
                reserve: source_row.current_reserve.clone(),
                market: Some(source_row.current_market.clone()),
                liquidity_mint: source_row.current_liquidity_mint.clone(),
                amount_raw: source_amount,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": seed.source,
                    "user_yield_position_id": source_row.id,
                    "seed_role": "source",
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                    "redeemable_source_liquidity_amount_raw": source_amount.to_string(),
                }),
            },
            ReconciledReservePosition {
                reserve: target_reserve,
                market: Some(target_market.to_owned()),
                liquidity_mint: source_row.current_liquidity_mint.clone(),
                amount_raw: target_amount,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": seed.source,
                    "user_yield_position_id": target_row.map(|row| row.id),
                    "seed_role": "target",
                    "amount_semantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
                    "redeemable_source_liquidity_amount_raw": target_amount.to_string(),
                }),
            },
        ],
    })
}

fn chain_preview_reconciled_state(
    preview: &ChainReconcilePreview,
) -> Result<ReconciledVaultState, Box<dyn Error>> {
    Ok(ReconciledVaultState {
        observed_slot: preview.observed_slot,
        observed_at: None,
        chain_slot: Some(preview.observed_slot),
        lock_attempt_id: None,
        context: json!({
            "kind": "same_mint_chain_reconcile_preview",
            "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
        }),
        positions: preview
            .positions
            .iter()
            .map(|position| ReconciledReservePosition {
                reserve: position.reserve.clone(),
                market: Some(position.market.clone()),
                liquidity_mint: position.liquidity_mint.clone(),
                amount_raw: position.amount_raw,
                supply_apy_bps: None,
                borrow_apy_bps: None,
                planning_metadata: json!({
                    "source": "chain_reconcile_preview",
                    "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                    "source_collateral_amount_raw": position.amount_raw.to_string(),
                    "redeemable_source_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                    "redeemable_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                    "obligation": position.obligation,
                    "obligation_exists": position.obligation_exists,
                    "vault_liquidity_ata": position.vault_liquidity_ata,
                    "vault_liquidity_token_account_exists": position.vault_liquidity_token_account_exists,
                    "idle_vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
                    "vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
                }),
            })
            .collect(),
    })
}

fn ensure_post_confirm_chain_reconcile_state(
    decision: &PreparedSameMintDecision,
    state: &ReconciledVaultState,
) -> Result<(), Box<dyn Error>> {
    let mut saw_source = false;
    let mut saw_target = false;

    for position in &state.positions {
        if position.reserve == decision.source_reserve {
            saw_source = true;
            if position.liquidity_mint != decision.liquidity_mint {
                return Err(format!(
                    "post-confirm source reserve liquidity mint {} does not match decision mint {}",
                    position.liquidity_mint, decision.liquidity_mint
                )
                .into());
            }
            if position.amount_raw != 0 {
                return Err(format!(
                    "post-confirm source reserve {} remains nonzero in chain reconcile: {}",
                    decision.source_reserve, position.amount_raw
                )
                .into());
            }
        } else if position.reserve == decision.target_reserve {
            saw_target = true;
            if position.liquidity_mint != decision.liquidity_mint {
                return Err(format!(
                    "post-confirm target reserve liquidity mint {} does not match decision mint {}",
                    position.liquidity_mint, decision.liquidity_mint
                )
                .into());
            }
            if position.amount_raw == 0 {
                return Err(format!(
                    "post-confirm target reserve {} is zero in chain reconcile",
                    decision.target_reserve
                )
                .into());
            }
        }
    }

    if !saw_source || !saw_target {
        return Err("post-confirm chain reconcile requires source and target positions".into());
    }

    Ok(())
}

fn target_market_for_seed(
    seed: &UserPositionSeedPreview,
    reserve_move: &ReserveMove,
    chain_preview: Option<&ChainReconcilePreview>,
    direction: Direction,
) -> Result<String, Box<dyn Error>> {
    let source_liquidity_mint = seed
        .rows
        .iter()
        .find(|row| row.current_reserve == reserve_move.source_reserve)
        .map(|row| row.current_liquidity_mint.as_str());
    if let Some(row) = seed.rows.iter().find(|row| {
        row.current_reserve == reserve_move.target_reserve
            && source_liquidity_mint.is_some_and(|mint| row.current_liquidity_mint == mint)
    }) {
        return Ok(row.current_market.clone());
    }
    if let Some(preview) = chain_preview {
        return Ok(
            chain_position_for_reserve(preview, &reserve_move.target_reserve)?
                .market
                .clone(),
        );
    }
    if reserve_move.target_reserve == direction.target_reserve() {
        return Ok(direction.target_market().to_owned());
    }
    Err(format!(
        "--seed-from-user-position with target reserve {} requires --reconcile-from-chain or an existing target row to determine the target market",
        reserve_move.target_reserve
    )
    .into())
}

fn amount_i64_to_u64(amount: i64, field: &str) -> Result<u64, Box<dyn Error>> {
    if amount < 0 {
        return Err(format!("{field} {amount} cannot be negative").into());
    }
    Ok(amount as u64)
}

fn load_chain_reconcile_preview(
    rpc_url: &str,
    vault: &SelectedVault,
    reserves: &[String],
) -> Result<ChainReconcilePreview, Box<dyn Error>> {
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let observed_slot = i64::try_from(rpc.get_slot()?)?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let (vault_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault_pubkey);
    let vault_user_metadata_exists =
        account_exists_with_owner(&rpc, &vault_user_metadata, &KLEND_PROGRAM_ID)?;
    let mut reserve_pubkeys = Vec::with_capacity(reserves.len());
    for reserve in reserves {
        let pubkey = Pubkey::from_str(reserve)
            .map_err(|_| format!("reconcile reserve {reserve} must be a public key"))?;
        if !reserve_pubkeys.iter().any(|existing| existing == &pubkey) {
            reserve_pubkeys.push(pubkey);
        }
    }
    let mut positions = Vec::with_capacity(reserve_pubkeys.len());

    let mut reserve_index = 0;
    while reserve_index < reserve_pubkeys.len() {
        let reserve = reserve_pubkeys[reserve_index];
        reserve_index += 1;
        let reserve_summary = load_kamino_reserve_summary(&rpc, &reserve)?;
        let vault_liquidity_ata = derive_associated_token_address(
            &vault_pubkey,
            &reserve_summary.liquidity_mint,
            &spl_token::ID,
        );
        let (vault_liquidity_amount_raw, vault_liquidity_token_account_exists) =
            load_spl_token_account_amount(
                &rpc,
                &vault_liquidity_ata,
                &reserve_summary.liquidity_mint,
            )?;

        let collateral_mint = reserve_summary.collateral_mint;
        let (obligation_account, _) = obligation(
            &KLEND_PROGRAM_ID,
            0,
            0,
            &vault_pubkey,
            &reserve_summary.market,
            &Pubkey::default(),
            &Pubkey::default(),
        );
        let obligation_summary = load_kamino_obligation_summary(
            &rpc,
            &obligation_account,
            &vault_pubkey,
            &reserve_summary.market,
            &reserve,
        )?;
        append_missing_obligation_reserve_pubkeys(
            &mut reserve_pubkeys,
            &obligation_summary,
            &obligation_account,
        )?;
        let (collateral_farm_user_state, collateral_farm_user_state_exists) =
            if let Some(collateral_farm) = reserve_summary.collateral_farm {
                let (farm_user_state, _) = farms_user_state(&collateral_farm, &obligation_account);
                let exists = account_exists_with_owner(&rpc, &farm_user_state, &FARMS_PROGRAM_ID)?;
                (Some(farm_user_state.to_string()), exists)
            } else {
                (None, false)
            };

        let redeemable_liquidity_amount_raw = collateral_to_redeemable_liquidity_amount(
            reserve_summary.collateral_total_supply,
            &reserve_summary.total_liquidity_scaled,
            obligation_summary.reserve_deposited_amount_raw,
        )?;

        positions.push(ChainPositionSummary {
            reserve: reserve.to_string(),
            market: reserve_summary.market.to_string(),
            liquidity_mint: reserve_summary.liquidity_mint.to_string(),
            liquidity_token_program: reserve_summary.liquidity_token_program.to_string(),
            reserve_liquidity_supply: reserve_summary.liquidity_supply.to_string(),
            collateral_mint: collateral_mint.to_string(),
            reserve_collateral_supply: reserve_summary.collateral_supply.to_string(),
            collateral_farm: reserve_summary.collateral_farm.map(|farm| farm.to_string()),
            collateral_farm_user_state,
            collateral_farm_user_state_exists,
            pyth_oracle: reserve_summary.pyth_oracle.map(|oracle| oracle.to_string()),
            switchboard_price_oracle: reserve_summary
                .switchboard_price_oracle
                .map(|oracle| oracle.to_string()),
            switchboard_twap_oracle: reserve_summary
                .switchboard_twap_oracle
                .map(|oracle| oracle.to_string()),
            scope_prices: reserve_summary
                .scope_prices
                .map(|account| account.to_string()),
            obligation: obligation_account.to_string(),
            obligation_exists: obligation_summary.exists,
            obligation_deposit_reserves: obligation_summary.deposit_reserves,
            obligation_borrow_reserves: obligation_summary.borrow_reserves,
            amount_raw: obligation_summary.reserve_deposited_amount_raw,
            redeemable_liquidity_amount_raw,
            vault_liquidity_ata: vault_liquidity_ata.to_string(),
            vault_liquidity_token_account_exists,
            vault_liquidity_amount_raw,
        });
    }

    Ok(ChainReconcilePreview {
        observed_slot,
        vault_user_metadata: vault_user_metadata.to_string(),
        vault_user_metadata_exists,
        positions,
    })
}

fn append_missing_obligation_reserve_pubkeys(
    reserve_pubkeys: &mut Vec<Pubkey>,
    obligation_summary: &KaminoObligationSummary,
    obligation_account: &Pubkey,
) -> Result<(), Box<dyn Error>> {
    for reserve in obligation_summary
        .deposit_reserves
        .iter()
        .chain(obligation_summary.borrow_reserves.iter())
    {
        let pubkey = Pubkey::from_str(reserve).map_err(|error| {
            format!(
                "invalid reserve {reserve} referenced by obligation {obligation_account}: {error}"
            )
        })?;
        if !reserve_pubkeys.iter().any(|existing| existing == &pubkey) {
            reserve_pubkeys.push(pubkey);
        }
    }

    Ok(())
}

fn load_policy_account_preflight(
    rpc_url: &str,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
) -> Result<PolicyAccountPreflight, Box<dyn Error>> {
    let source = chain_position_for_reserve(preview, &reserve_move.source_reserve)?;
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve)?;
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let account = rpc.get_account(&policy_account)?;
    let decoded = decode_squads_policy_account(&account.data).map_err(|error| {
        format!(
            "failed to decode Squads policy account {}: {error}",
            vault.policy_account
        )
    })?;

    Ok(PolicyAccountPreflight {
        policy_account: vault.policy_account.clone(),
        source_market: source.market.clone(),
        target_market: target.market.clone(),
        decoded,
    })
}

fn decode_squads_policy_account(data: &[u8]) -> Result<DecodedPolicyAccount, String> {
    let mut cursor = PolicyCursor::new(data);
    let discriminator = cursor.read_array::<8>()?;
    if discriminator != SQUADS_POLICY_ACCOUNT_DISCRIMINATOR {
        return Err("account discriminator is not a Squads Policy account".to_owned());
    }
    cursor.skip(PUBKEY_LEN)?;
    cursor.skip(8)?;
    cursor.skip(1)?;
    cursor.skip(8)?;
    cursor.skip(8)?;

    let signer_count = cursor.read_u32_len("policy signer count", 32)?;
    let mut delegated_signers = Vec::with_capacity(signer_count);
    for _ in 0..signer_count {
        delegated_signers.push(cursor.read_pubkey()?.to_string());
        cursor.skip(1)?;
    }
    let threshold = cursor.read_u16()?;
    cursor.skip(4)?;

    let policy_state_tag = cursor.read_u8()?;
    if policy_state_tag != 3 {
        return Err(format!(
            "unsupported policy state tag {policy_state_tag}; expected ProgramInteraction (3)"
        ));
    }
    let layout = PolicyAccountLayout::ProgramInteractionPolicyState;
    let account_index = cursor.read_u8()?;
    let legacy_cursor = cursor.clone();
    let constraints = match read_legacy_program_interaction_instruction_constraints(cursor) {
        Ok(constraints) => constraints,
        Err(legacy_error) => {
            let mut compact_cursor = legacy_cursor;
            read_compact_program_interaction_instruction_constraints(&mut compact_cursor)
                .map_err(|compact_error| {
                    format!(
                        "failed to decode ProgramInteraction policy as legacy ({legacy_error}) or compact ({compact_error})"
                    )
                })?
        }
    };

    Ok(summarize_policy_account(
        layout,
        delegated_signers,
        threshold,
        account_index,
        constraints,
    ))
}

fn read_legacy_program_interaction_instruction_constraints(
    mut cursor: PolicyCursor<'_>,
) -> Result<Vec<PolicyInstructionConstraint>, String> {
    let len = cursor.read_u32_len("program interaction instruction constraint count", 128)?;
    read_program_interaction_instruction_constraints(&mut cursor, len)
}

fn summarize_policy_account(
    layout: PolicyAccountLayout,
    delegated_signers: Vec<String>,
    threshold: u16,
    account_index: u8,
    constraints: Vec<PolicyInstructionConstraint>,
) -> DecodedPolicyAccount {
    let mut kamino_markets = Vec::new();
    let mut kamino_liquidity_mints = Vec::new();
    let mut instructions = Vec::with_capacity(constraints.len());
    let instruction_count = constraints.len();

    for constraint in &constraints {
        let discriminator = instruction_discriminator(&constraint);
        let route_step = kamino_route_step(&constraint, discriminator.as_deref());
        let markets = if let Some(step) = route_step {
            let account_index = match step {
                KAMINO_WITHDRAW_ROUTE_STEP | KAMINO_DEPOSIT_ROUTE_STEP => 2,
                KAMINO_INIT_OBLIGATION_ROUTE_STEP => 3,
                KAMINO_REFRESH_OBLIGATION_ROUTE_STEP => 0,
                _ => 1,
            };
            pubkeys_for_account(&constraint, account_index).unwrap_or_default()
        } else if constraint.program_id == KLEND_PROGRAM_ID {
            let mut markets = pubkeys_for_account(&constraint, 1).unwrap_or_default();
            markets.extend(pubkeys_for_account(&constraint, 2).unwrap_or_default());
            unique_pubkeys(markets)
        } else {
            Vec::new()
        }
        .into_iter()
        .map(|pubkey| pubkey.to_string())
        .collect::<Vec<_>>();
        let liquidity_mints = if route_step == Some(KAMINO_WITHDRAW_ROUTE_STEP)
            || route_step == Some(KAMINO_DEPOSIT_ROUTE_STEP)
            || (route_step.is_none() && constraint.program_id == KLEND_PROGRAM_ID)
        {
            let mut liquidity_mints = pubkeys_for_account(&constraint, 5).unwrap_or_default();
            liquidity_mints.extend(account_data_pubkeys_for_account(
                &constraint,
                5,
                SPL_TOKEN_ACCOUNT_MINT_OFFSET as u64,
                Some(spl_token::ID),
            ));
            unique_pubkeys(liquidity_mints)
        } else {
            Vec::new()
        }
        .into_iter()
        .map(|pubkey| pubkey.to_string())
        .collect::<Vec<_>>();

        extend_unique_strings(&mut kamino_markets, &markets);
        extend_unique_strings(&mut kamino_liquidity_mints, &liquidity_mints);

        instructions.push(DecodedPolicyInstructionSummary {
            program_id: constraint.program_id.to_string(),
            route_step,
            data_discriminator: discriminator,
            markets,
            liquidity_mints,
            account_constraints: decoded_policy_account_constraint_summaries(
                &constraint.account_constraints,
            ),
        });
    }

    DecodedPolicyAccount {
        layout,
        delegated_signers,
        threshold,
        account_index,
        instruction_count,
        kamino_markets,
        kamino_liquidity_mints,
        constraints,
        instructions,
    }
}

fn decoded_policy_account_constraint_summaries(
    constraints: &[PolicyAccountConstraint],
) -> Vec<DecodedPolicyAccountConstraintSummary> {
    constraints
        .iter()
        .map(|constraint| DecodedPolicyAccountConstraintSummary {
            account_index: constraint.account_index,
            kind: if constraint.pubkeys.is_empty() {
                "account_data"
            } else {
                "pubkey"
            },
            pubkeys: constraint.pubkeys.iter().map(ToString::to_string).collect(),
            owner: constraint.owner.map(|owner| owner.to_string()),
            data_constraints: constraint
                .data_constraints
                .iter()
                .map(decoded_policy_data_constraint_summary)
                .collect(),
        })
        .collect()
}

fn decoded_policy_data_constraint_summary(
    constraint: &PolicyDataConstraint,
) -> DecodedPolicyDataConstraintSummary {
    DecodedPolicyDataConstraintSummary {
        data_offset: constraint.data_offset,
        operator: constraint.operator.as_str(),
        value: constraint.data_value.to_json(),
    }
}

fn read_program_interaction_instruction_constraints(
    cursor: &mut PolicyCursor<'_>,
    len: usize,
) -> Result<Vec<PolicyInstructionConstraint>, String> {
    let mut constraints = Vec::with_capacity(len);
    for _ in 0..len {
        let program_id = cursor.read_pubkey()?;
        let account_constraint_count =
            cursor.read_u32_len("program interaction account constraint count", 128)?;
        let account_constraints =
            read_program_interaction_account_constraints(cursor, account_constraint_count)?;
        let data_constraint_count =
            cursor.read_u32_len("program interaction data constraint count", 128)?;
        let data_constraints = read_policy_data_constraints(cursor, data_constraint_count)?;
        constraints.push(PolicyInstructionConstraint {
            program_id,
            account_constraints,
            data_constraints,
        });
    }
    Ok(constraints)
}

fn read_compact_program_interaction_instruction_constraints(
    cursor: &mut PolicyCursor<'_>,
) -> Result<Vec<PolicyInstructionConstraint>, String> {
    let pubkey_table_len = cursor.read_u8()? as usize;
    if pubkey_table_len > 240 {
        return Err(format!(
            "program interaction pubkey table length {pubkey_table_len} exceeds maximum 240"
        ));
    }
    let pubkey_table = (0..pubkey_table_len)
        .map(|_| cursor.read_pubkey())
        .collect::<Result<Vec<_>, _>>()?;
    let instruction_count = cursor.read_u8()? as usize;
    if instruction_count > 128 {
        return Err(format!(
            "program interaction instruction constraint count {instruction_count} exceeds maximum 128"
        ));
    }
    let mut constraints = Vec::with_capacity(instruction_count);
    for _ in 0..instruction_count {
        let program_id = compact_pubkey(&pubkey_table, cursor.read_u8()?)?;
        let account_constraint_count = cursor.read_u8()? as usize;
        if account_constraint_count > 128 {
            return Err(format!(
                "program interaction account constraint count {account_constraint_count} exceeds maximum 128"
            ));
        }
        let mut account_constraints = Vec::with_capacity(account_constraint_count);
        for _ in 0..account_constraint_count {
            let account_index = cursor.read_u8()?;
            let (pubkeys, data_constraints) = match cursor.read_u8()? {
                0 => {
                    let len = cursor.read_u8()? as usize;
                    if len > 128 {
                        return Err(format!(
                            "program interaction pubkey account constraint {len} exceeds maximum 128"
                        ));
                    }
                    let mut pubkeys = Vec::with_capacity(len);
                    for _ in 0..len {
                        pubkeys.push(compact_pubkey(&pubkey_table, cursor.read_u8()?)?);
                    }
                    (pubkeys, Vec::new())
                }
                1 => {
                    let len = cursor.read_u8()? as usize;
                    if len > 128 {
                        return Err(format!(
                            "program interaction account data constraint count {len} exceeds maximum 128"
                        ));
                    }
                    (Vec::new(), read_policy_data_constraints(cursor, len)?)
                }
                tag => {
                    return Err(format!(
                        "unknown compact program interaction account constraint kind {tag}"
                    ))
                }
            };
            let owner = match cursor.read_u8()? {
                0 => None,
                1 => Some(compact_pubkey(&pubkey_table, cursor.read_u8()?)?),
                tag => return Err(format!("invalid compact pubkey option tag {tag}")),
            };
            account_constraints.push(PolicyAccountConstraint {
                account_index,
                pubkeys,
                data_constraints,
                owner,
            });
        }
        let data_constraint_count = cursor.read_u8()? as usize;
        if data_constraint_count > 128 {
            return Err(format!(
                "program interaction data constraint count {data_constraint_count} exceeds maximum 128"
            ));
        }
        let data_constraints = read_policy_data_constraints(cursor, data_constraint_count)?;
        constraints.push(PolicyInstructionConstraint {
            program_id,
            account_constraints,
            data_constraints,
        });
    }
    Ok(constraints)
}

fn compact_pubkey(pubkey_table: &[Pubkey], index: u8) -> Result<Pubkey, String> {
    pubkey_table
        .get(index as usize)
        .copied()
        .ok_or_else(|| format!("compact pubkey table index {index} is out of bounds"))
}

fn read_program_interaction_account_constraints(
    cursor: &mut PolicyCursor<'_>,
    len: usize,
) -> Result<Vec<PolicyAccountConstraint>, String> {
    let mut constraints = Vec::with_capacity(len);
    for _ in 0..len {
        let account_index = cursor.read_u8()?;
        let (pubkeys, data_constraints) = match cursor.read_u8()? {
            0 => (
                cursor.read_pubkey_vec_u32("program interaction pubkey account constraint", 128)?,
                Vec::new(),
            ),
            1 => (Vec::new(), {
                let len = cursor
                    .read_u32_len("program interaction account data constraint count", 128)?;
                read_policy_data_constraints(cursor, len)?
            }),
            tag => {
                return Err(format!(
                    "unknown program interaction account constraint kind {tag}"
                ))
            }
        };
        let owner = cursor.read_option_pubkey()?;
        constraints.push(PolicyAccountConstraint {
            account_index,
            pubkeys,
            data_constraints,
            owner,
        });
    }
    Ok(constraints)
}

fn read_policy_data_constraints(
    cursor: &mut PolicyCursor<'_>,
    len: usize,
) -> Result<Vec<PolicyDataConstraint>, String> {
    let mut constraints = Vec::with_capacity(len);
    for _ in 0..len {
        constraints.push(PolicyDataConstraint {
            data_offset: cursor.read_u64()?,
            data_value: match cursor.read_u8()? {
                0 => PolicyDataValue::U8(cursor.read_u8()?),
                1 => PolicyDataValue::U16Le(cursor.read_u16()?),
                2 => PolicyDataValue::U32Le(cursor.read_u32()?),
                3 => PolicyDataValue::U64Le(cursor.read_u64()?),
                4 => PolicyDataValue::U128Le(cursor.read_u128()?),
                5 => PolicyDataValue::U8Slice(cursor.read_vec_u8("data u8 slice", 256)?),
                tag => return Err(format!("unknown data value kind {tag}")),
            },
            operator: match cursor.read_u8()? {
                0 => PolicyDataOperator::Equals,
                1 => PolicyDataOperator::NotEquals,
                2 => PolicyDataOperator::GreaterThan,
                3 => PolicyDataOperator::GreaterThanOrEqualTo,
                4 => PolicyDataOperator::LessThan,
                5 => PolicyDataOperator::LessThanOrEqualTo,
                tag => return Err(format!("unknown data operator {tag}")),
            },
        });
    }
    Ok(constraints)
}

fn instruction_discriminator(constraint: &PolicyInstructionConstraint) -> Option<Vec<u8>> {
    constraint
        .data_constraints
        .iter()
        .find_map(|data_constraint| {
            if data_constraint.data_offset == 0
                && data_constraint.operator == PolicyDataOperator::Equals
                && matches!(data_constraint.data_value, PolicyDataValue::U8Slice(_))
            {
                if let PolicyDataValue::U8Slice(value) = &data_constraint.data_value {
                    return Some(value.clone());
                }
            }
            None
        })
}

fn kamino_route_step(
    constraint: &PolicyInstructionConstraint,
    discriminator: Option<&[u8]>,
) -> Option<&'static str> {
    if constraint.program_id != KLEND_PROGRAM_ID {
        return None;
    }
    match discriminator {
        Some(value)
            if value
                .starts_with(&WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2) =>
        {
            Some(KAMINO_WITHDRAW_ROUTE_STEP)
        }
        Some(value)
            if value.starts_with(&DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2) =>
        {
            Some(KAMINO_DEPOSIT_ROUTE_STEP)
        }
        Some(value) if value.starts_with(&INIT_OBLIGATION) => {
            Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)
        }
        Some(value) if value.starts_with(&REFRESH_OBLIGATION) => {
            Some(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP)
        }
        _ => None,
    }
}

fn pubkeys_for_account(
    constraint: &PolicyInstructionConstraint,
    account_index: u8,
) -> Option<Vec<Pubkey>> {
    constraint
        .account_constraints
        .iter()
        .find(|constraint| constraint.account_index == account_index)
        .map(|constraint| constraint.pubkeys.clone())
}

fn account_data_pubkeys_for_account(
    constraint: &PolicyInstructionConstraint,
    account_index: u8,
    data_offset: u64,
    owner: Option<Pubkey>,
) -> Vec<Pubkey> {
    constraint
        .account_constraints
        .iter()
        .filter(|constraint| constraint.account_index == account_index && constraint.owner == owner)
        .flat_map(|constraint| {
            constraint
                .data_constraints
                .iter()
                .filter_map(move |data_constraint| {
                    if data_constraint.data_offset == data_offset
                        && data_constraint.operator == PolicyDataOperator::Equals
                    {
                        if let PolicyDataValue::U8Slice(value) = &data_constraint.data_value {
                            return value.as_slice().try_into().ok().map(Pubkey::new_from_array);
                        }
                    }
                    None
                })
        })
        .collect()
}

fn unique_pubkeys(pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for pubkey in pubkeys {
        if !unique.contains(&pubkey) {
            unique.push(pubkey);
        }
    }
    unique
}

fn extend_unique_strings(target: &mut Vec<String>, values: &[String]) {
    for value in values {
        if !target.contains(value) {
            target.push(value.clone());
        }
    }
}

#[derive(Clone)]
struct PolicyCursor<'a> {
    data: &'a [u8],
    offset: usize,
}

impl<'a> PolicyCursor<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, offset: 0 }
    }

    fn remaining(&self) -> usize {
        self.data.len().saturating_sub(self.offset)
    }

    fn skip(&mut self, len: usize) -> Result<(), String> {
        self.take(len).map(|_| ())
    }

    fn take(&mut self, len: usize) -> Result<&'a [u8], String> {
        if self.remaining() < len {
            return Err(format!(
                "truncated policy account data at offset {}, need {len} bytes",
                self.offset
            ));
        }
        let start = self.offset;
        self.offset += len;
        Ok(&self.data[start..self.offset])
    }

    fn read_array<const N: usize>(&mut self) -> Result<[u8; N], String> {
        self.take(N)?
            .try_into()
            .map_err(|_| "slice length mismatch".to_owned())
    }

    fn read_u8(&mut self) -> Result<u8, String> {
        Ok(self.take(1)?[0])
    }

    fn read_u16(&mut self) -> Result<u16, String> {
        Ok(u16::from_le_bytes(self.read_array()?))
    }

    fn read_u32(&mut self) -> Result<u32, String> {
        Ok(u32::from_le_bytes(self.read_array()?))
    }

    fn read_u64(&mut self) -> Result<u64, String> {
        Ok(u64::from_le_bytes(self.read_array()?))
    }

    fn read_u128(&mut self) -> Result<u128, String> {
        Ok(u128::from_le_bytes(self.read_array()?))
    }

    fn read_u32_len(&mut self, label: &str, max: usize) -> Result<usize, String> {
        let len = self.read_u32()? as usize;
        if len > max {
            return Err(format!("{label} {len} exceeds maximum {max}"));
        }
        Ok(len)
    }

    fn read_pubkey(&mut self) -> Result<Pubkey, String> {
        Ok(Pubkey::new_from_array(self.read_array()?))
    }

    fn read_vec_u8(&mut self, label: &str, max: usize) -> Result<Vec<u8>, String> {
        let len = self.read_u32_len(label, max)?;
        Ok(self.take(len)?.to_vec())
    }

    fn read_pubkey_vec_u32(&mut self, label: &str, max: usize) -> Result<Vec<Pubkey>, String> {
        let len = self.read_u32_len(label, max)?;
        (0..len).map(|_| self.read_pubkey()).collect()
    }

    fn read_option_pubkey(&mut self) -> Result<Option<Pubkey>, String> {
        match self.read_u8()? {
            0 => Ok(None),
            1 => self.read_pubkey().map(Some),
            tag => Err(format!("invalid pubkey option tag {tag}")),
        }
    }
}

fn chain_position_for_reserve<'a>(
    preview: &'a ChainReconcilePreview,
    reserve: &str,
) -> Result<&'a ChainPositionSummary, Box<dyn Error>> {
    preview
        .positions
        .iter()
        .find(|position| position.reserve == reserve)
        .ok_or_else(|| format!("chain preview missing required reserve {reserve}").into())
}

fn push_obligation_refresh_position<'a>(
    preview: &'a ChainReconcilePreview,
    seen: &mut BTreeSet<String>,
    positions: &mut Vec<&'a ChainPositionSummary>,
    reserve: &str,
    context: &str,
) -> Result<(), Box<dyn Error>> {
    let reserve = Pubkey::from_str(reserve)
        .map_err(|error| format!("invalid obligation refresh reserve {reserve}: {error}"))?
        .to_string();
    if !seen.insert(reserve.clone()) {
        return Ok(());
    }

    let position = chain_position_for_reserve(preview, &reserve).map_err(|_| {
        format!(
            "missing_obligation_refresh_reserve_metadata reserve {reserve} referenced by {context}; chain preview lacks metadata needed to build Kamino RefreshReserve"
        )
    })?;
    positions.push(position);
    Ok(())
}

fn obligation_refresh_positions_for_route<'a>(
    preview: &'a ChainReconcilePreview,
    source: &'a ChainPositionSummary,
    target: &'a ChainPositionSummary,
) -> Result<Vec<&'a ChainPositionSummary>, Box<dyn Error>> {
    let mut seen = BTreeSet::new();
    let mut positions = Vec::new();

    push_obligation_refresh_position(
        preview,
        &mut seen,
        &mut positions,
        &source.reserve,
        "selected source reserve",
    )?;
    push_obligation_refresh_position(
        preview,
        &mut seen,
        &mut positions,
        &target.reserve,
        "selected target reserve",
    )?;

    let source_deposit_context =
        format!("source obligation {} deposit reserves", source.obligation);
    for reserve in &source.obligation_deposit_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &source_deposit_context,
        )?;
    }
    let source_borrow_context = format!("source obligation {} borrow reserves", source.obligation);
    for reserve in &source.obligation_borrow_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &source_borrow_context,
        )?;
    }
    let target_deposit_context =
        format!("target obligation {} deposit reserves", target.obligation);
    for reserve in &target.obligation_deposit_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &target_deposit_context,
        )?;
    }
    let target_borrow_context = format!("target obligation {} borrow reserves", target.obligation);
    for reserve in &target.obligation_borrow_reserves {
        push_obligation_refresh_position(
            preview,
            &mut seen,
            &mut positions,
            reserve,
            &target_borrow_context,
        )?;
    }

    Ok(positions)
}

fn execution_preflight_blocker(
    chain_preview: Option<&ChainReconcilePreview>,
    policy_preflight: Option<&PolicyAccountPreflight>,
    reserve_move: &ReserveMove,
    route_execution: Option<&RouteExecutionPlan>,
) -> Option<String> {
    execution_preflight_blockers(
        chain_preview,
        policy_preflight,
        reserve_move,
        route_execution,
    )
    .into_iter()
    .next()
}

fn execution_preflight_blockers(
    chain_preview: Option<&ChainReconcilePreview>,
    policy_preflight: Option<&PolicyAccountPreflight>,
    reserve_move: &ReserveMove,
    route_execution: Option<&RouteExecutionPlan>,
) -> Vec<String> {
    let Some(chain_preview) = chain_preview else {
        return vec!["--execute requires --reconcile-from-chain".to_owned()];
    };

    let mut blockers = Vec::new();
    match chain_position_for_reserve(chain_preview, &reserve_move.source_reserve) {
        Ok(source) => {
            if !source.obligation_exists {
                blockers.push(format!(
                    "source obligation account {} does not exist",
                    source.obligation
                ));
            }
            if source.amount_raw == 0 {
                blockers.push(format!(
                    "source obligation account {} has zero deposited amount for reserve {}",
                    source.obligation, source.reserve
                ));
            }
            if !source.vault_liquidity_token_account_exists {
                blockers.push(format!(
                    "vault liquidity token account {} does not exist",
                    source.vault_liquidity_ata
                ));
            }
        }
        Err(error) => blockers.push(error.to_string()),
    }
    match chain_position_for_reserve(chain_preview, &reserve_move.target_reserve) {
        Ok(target) => {
            if !target.obligation_exists {
                match route_execution {
                    Some(plan) if plan.preview.missing_obligation_setup.is_some() => {}
                    Some(_) => blockers.push(format!(
                        "target obligation account {} does not exist and no inline init_obligation route step is planned before same-mint deposit",
                        target.obligation
                    )),
                    None => {}
                }
            }
        }
        Err(error) => blockers.push(error.to_string()),
    }
    if let Some(policy_preflight) = policy_preflight {
        let mut missing = Vec::new();
        if !policy_preflight.allows_required_route_steps() {
            missing.push("required same-mint KLend route steps");
        }
        if decoded_route_instruction_constraint_indexes(&policy_preflight.decoded).is_err() {
            missing.push("usable same-mint instruction constraint indexes");
        }
        if !policy_preflight.allows_required_markets() {
            missing.push("both required markets");
        }
        if !missing.is_empty() {
            blockers.push(format!(
                "decoded policy account does not allow {}",
                missing.join(" and ")
            ));
        }
    }
    if let Some(validation) =
        route_execution.and_then(|plan| plan.preview.policy_constraint_validation.as_ref())
    {
        if !validation.matches {
            blockers.push(format!(
                "decoded policy account constraints do not match built KLend v2 route: {}",
                validation.failures.join("; ")
            ));
        }
    }
    blockers
}

fn writes_current_positions_from_chain(options: &CliOptions) -> bool {
    options.execute && options.reconcile_from_chain && !options.provision_route_lookup_table
}

fn writes_current_positions_from_user_seed(options: &CliOptions) -> bool {
    options.execute && options.seed_from_user_position && !options.provision_route_lookup_table
}

fn uses_chain_preview_positions(options: &CliOptions, has_chain_preview: bool) -> bool {
    has_chain_preview
        && options.reconcile_from_chain
        && (!options.execute || options.provision_route_lookup_table)
}

fn same_mint_route_fee_payer_pubkey(options: &CliOptions) -> Result<Pubkey, Box<dyn Error>> {
    if options.optimization_cycle || options.provision_route_lookup_table {
        Ok(policy_keypair_from_env()?.pubkey())
    } else {
        Ok(solana_testing_keypair_from_env()?.pubkey())
    }
}

fn same_mint_route_signer_pubkeys(fee_payer: Pubkey, delegated_signer: Pubkey) -> Vec<Pubkey> {
    let mut signer_pubkeys = vec![fee_payer, delegated_signer];
    signer_pubkeys.sort();
    signer_pubkeys.dedup();
    signer_pubkeys
}

fn same_mint_route_signers<'a>(
    fee_payer: &'a dyn Signer,
    delegated_signer: &'a dyn Signer,
) -> Vec<&'a dyn Signer> {
    if fee_payer.pubkey() == delegated_signer.pubkey() {
        vec![fee_payer]
    } else {
        vec![fee_payer, delegated_signer]
    }
}

fn build_program_interaction_policy_execution_instruction(
    policy: Pubkey,
    signer_pubkey: Pubkey,
    account_index: u8,
    instruction: Instruction,
    instruction_constraint_index: u8,
) -> (Instruction, usize, usize, usize) {
    let mut transaction_accounts = Vec::new();
    let compiled_instruction =
        compile_squads_inner_instruction(&mut transaction_accounts, instruction);
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy,
        signer_pubkey,
        account_index,
        vec![compiled_instruction],
        vec![instruction_constraint_index],
        transaction_accounts.clone(),
    );
    (
        outer_instruction.clone(),
        1,
        transaction_accounts.len(),
        outer_instruction.accounts.len(),
    )
}

fn planned_source_collateral_amount(
    input: &SameMintRebalanceInput,
    source: &ChainPositionSummary,
) -> Result<u64, Box<dyn Error>> {
    let Some(source_collateral_amount_raw) = input.source_collateral_amount_raw else {
        return Err(
            "planned same-mint route is missing source_collateral_amount_raw for Kamino withdraw"
                .into(),
        );
    };
    let source_collateral_amount =
        amount_i64_to_u64(source_collateral_amount_raw, "source collateral amount")?;
    if source_collateral_amount == 0 {
        return Err("source collateral amount must be greater than 0".into());
    }
    if source.amount_raw != source_collateral_amount {
        return Err(format!(
            "chain source reserve {} collateral amount {} does not match planned source_collateral_amount_raw {}",
            source.reserve, source.amount_raw, source_collateral_amount
        )
        .into());
    }
    Ok(source_collateral_amount)
}

fn build_route_execution_plan(
    rpc: Option<&RpcClient>,
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
    input: &SameMintRebalanceInput,
    policy_preflight: Option<&PolicyAccountPreflight>,
    fee_payer: Pubkey,
) -> Result<RouteExecutionPlan, Box<dyn Error>> {
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    let signer_pubkey = policy_keypair_from_env()?.pubkey();
    if let Some(policy_preflight) = policy_preflight {
        if !policy_preflight
            .decoded
            .delegated_signers
            .iter()
            .any(|signer| signer == &signer_pubkey.to_string())
        {
            return Err(format!(
                "decoded policy account {} does not allow POLICY_KEYPAIR signer {}",
                vault.policy_account, signer_pubkey
            )
            .into());
        }
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let account_index = u8::try_from(vault.vault_index).map_err(|_| {
        format!(
            "vault index {} does not fit Squads account index",
            vault.vault_index
        )
    })?;
    let route_liquidity_amount = amount_i64_to_u64(input.amount_raw, "route liquidity amount")?;
    let source = chain_position_for_reserve(preview, &reserve_move.source_reserve)?;
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve)?;
    let source_collateral_amount = planned_source_collateral_amount(input, source)?;
    if input.redeemable_source_liquidity_amount_raw != Some(input.amount_raw) {
        return Err(format!(
            "planned redeemable_source_liquidity_amount_raw {:?} does not match route amount {}",
            input.redeemable_source_liquidity_amount_raw, input.amount_raw
        )
        .into());
    }
    if source.redeemable_liquidity_amount_raw != route_liquidity_amount {
        return Err(format!(
            "chain source reserve {} redeemable liquidity amount {} does not match planned route amount {}",
            source.reserve, source.redeemable_liquidity_amount_raw, route_liquidity_amount
        )
        .into());
    }
    if !vault
        .stable_mints
        .iter()
        .any(|mint| mint == &input.liquidity_mint)
    {
        return Err(format!(
            "selected policy {} does not allow stable mint {}",
            vault.policy_account, input.liquidity_mint
        )
        .into());
    }
    if !vault
        .kamino_liquidity_mints
        .iter()
        .any(|mint| mint == &input.liquidity_mint)
    {
        return Err(format!(
            "selected policy {} does not allow Kamino liquidity mint {}",
            vault.policy_account, input.liquidity_mint
        )
        .into());
    }
    if source.liquidity_mint != input.liquidity_mint {
        return Err(format!(
            "source reserve {} liquidity mint {} does not match planned mint {}",
            source.reserve, source.liquidity_mint, input.liquidity_mint
        )
        .into());
    }
    if target.liquidity_mint != input.liquidity_mint {
        return Err(format!(
            "target reserve {} liquidity mint {} does not match planned mint {}",
            target.reserve, target.liquidity_mint, input.liquidity_mint
        )
        .into());
    }
    let planned_liquidity_mint = Pubkey::from_str(&input.liquidity_mint)?;
    let vault_liquidity_ata =
        derive_associated_token_address(&vault_pubkey, &planned_liquidity_mint, &spl_token::ID);

    let refresh_positions = obligation_refresh_positions_for_route(preview, source, target)?;
    let refresh_reserves = refresh_positions
        .iter()
        .map(|position| position.reserve.clone())
        .collect::<Vec<_>>();
    let source_farm_init_instruction =
        kamino_init_obligation_collateral_farm_instruction(fee_payer, vault_pubkey, source)?;
    let target_farm_init_instruction =
        kamino_init_obligation_collateral_farm_instruction(fee_payer, vault_pubkey, target)?;
    let source_refresh_instruction = kamino_refresh_obligation_instruction(source)?;
    let target_refresh_instruction = kamino_refresh_obligation_instruction(target)?;
    let source_instruction = kamino_withdraw_instruction(
        vault_pubkey,
        source,
        vault_liquidity_ata,
        source_collateral_amount,
    )?;
    let target_instruction = kamino_deposit_to_obligation_instruction(
        vault_pubkey,
        target,
        vault_liquidity_ata,
        route_liquidity_amount,
    )?;
    let source_instruction_program = source_instruction.program_id.to_string();
    let target_instruction_program = target_instruction.program_id.to_string();
    let source_instruction_discriminator = source_instruction.data[..8].to_vec();
    let target_instruction_discriminator = target_instruction.data[..8].to_vec();
    let instruction_constraint_indexes =
        route_instruction_constraint_indexes(vault, policy_preflight)?;
    let withdraw_instruction_constraint_index = instruction_constraint_indexes
        .first()
        .copied()
        .ok_or("route policy is missing withdraw instruction constraint index")?;
    let deposit_instruction_constraint_index = instruction_constraint_indexes
        .get(1)
        .copied()
        .ok_or("route policy is missing deposit instruction constraint index")?;
    let policy_constraint_validation = policy_preflight.map(|policy_preflight| {
        let route = [
            (KAMINO_WITHDRAW_ROUTE_STEP, &source_instruction),
            (KAMINO_DEPOSIT_ROUTE_STEP, &target_instruction),
        ];
        validate_route_policy_constraints(
            &policy_preflight.decoded,
            &instruction_constraint_indexes,
            &route,
        )
    });

    let (
        withdraw_outer_instruction,
        withdraw_inner_count,
        withdraw_transaction_account_count,
        withdraw_outer_account_count,
    ) = build_program_interaction_policy_execution_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        source_instruction,
        withdraw_instruction_constraint_index,
    );
    let (
        deposit_outer_instruction,
        deposit_inner_count,
        deposit_transaction_account_count,
        deposit_outer_account_count,
    ) = build_program_interaction_policy_execution_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        target_instruction,
        deposit_instruction_constraint_index,
    );

    let mut pre_instructions = refresh_positions
        .iter()
        .map(|position| kamino_refresh_reserve_instruction(position))
        .collect::<Result<Vec<_>, _>>()?;
    pre_instructions.extend(source_farm_init_instruction);
    pre_instructions.push(source_refresh_instruction);

    let mut protected_and_public_instructions = vec![withdraw_outer_instruction];
    let mut route_steps = vec![KAMINO_WITHDRAW_ROUTE_STEP];
    let mut inner_instruction_count = withdraw_inner_count;
    let mut transaction_account_count = withdraw_transaction_account_count;
    let mut outer_account_count = withdraw_outer_account_count;
    let mut missing_obligation_setup = None;
    let mut setup_policy_account = None;
    let mut init_instruction_constraint_index = None;
    let mut setup_instruction_program = None;
    let mut setup_instruction_discriminator = None;

    if target.obligation_exists {
        pre_instructions.extend(target_farm_init_instruction);
        pre_instructions.push(target_refresh_instruction);
        protected_and_public_instructions.push(kamino_refresh_obligation_for_reserves_instruction(
            target,
            &[target.reserve.as_str()],
        )?);
        route_steps.push(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP);
    } else {
        let (init_policy, init_index) =
            resolve_init_obligation_policy(rpc, vault, target, policy_preflight)?;
        let route_policy = Pubkey::from_str(&vault.policy_account)?;
        let policy_source = if init_policy == route_policy {
            "route_policy"
        } else {
            "setup_policy"
        };
        let init_instruction = kamino_init_obligation_instruction(vault_pubkey, target)?;
        setup_instruction_program = Some(init_instruction.program_id.to_string());
        setup_instruction_discriminator = Some(init_instruction.data[..8].to_vec());
        let (
            init_outer_instruction,
            init_inner_count,
            init_transaction_account_count,
            init_outer_account_count,
        ) = build_program_interaction_policy_execution_instruction(
            init_policy,
            signer_pubkey,
            account_index,
            init_instruction,
            init_index,
        );
        protected_and_public_instructions.push(init_outer_instruction);
        protected_and_public_instructions.extend(target_farm_init_instruction);
        protected_and_public_instructions.push(target_refresh_instruction);
        route_steps.push(KAMINO_INIT_OBLIGATION_ROUTE_STEP);
        route_steps.push(KAMINO_REFRESH_OBLIGATION_ROUTE_STEP);
        inner_instruction_count += init_inner_count;
        transaction_account_count += init_transaction_account_count;
        outer_account_count += init_outer_account_count;
        if policy_source == "setup_policy" {
            setup_policy_account = Some(init_policy.to_string());
        }
        init_instruction_constraint_index = Some(init_index);
        missing_obligation_setup = Some(InlineMissingObligationSetupPreview {
            target_obligation: target.obligation.clone(),
            target_reserve: target.reserve.clone(),
            target_market: target.market.clone(),
            policy_account: init_policy.to_string(),
            policy_source,
            instruction_constraint_index: init_index,
        });
    }

    protected_and_public_instructions.push(deposit_outer_instruction);
    route_steps.push(KAMINO_DEPOSIT_ROUTE_STEP);
    inner_instruction_count += deposit_inner_count;
    transaction_account_count += deposit_transaction_account_count;
    outer_account_count += deposit_outer_account_count;

    Ok(RouteExecutionPlan {
        pre_instructions,
        instructions: protected_and_public_instructions,
        preview: RouteExecutionPreview {
            policy_account: policy_account.to_string(),
            setup_policy_account,
            fee_payer: fee_payer.to_string(),
            signer: signer_pubkey.to_string(),
            account_index,
            instruction_constraint_indexes,
            init_instruction_constraint_index,
            policy_constraint_validation,
            missing_obligation_setup,
            setup_instruction_program,
            setup_instruction_discriminator,
            route_steps,
            refresh_reserves,
            inner_instruction_count,
            transaction_account_count,
            outer_account_count,
            source_instruction_program,
            target_instruction_program,
            source_instruction_discriminator,
            target_instruction_discriminator,
        },
    })
}

fn build_initial_reserve_deposit_policy_plan(
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    deposit_reserve: &str,
    amount: u64,
    payer_pubkey: Pubkey,
    signer_pubkey: Pubkey,
    account_index: u8,
) -> Result<InitialDepositPolicyPlan, Box<dyn Error>> {
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    if let Some(policy_preflight) = policy_preflight {
        if !policy_preflight
            .decoded
            .delegated_signers
            .iter()
            .any(|signer| signer == &signer_pubkey.to_string())
        {
            return Err(format!(
                "decoded policy account {} does not allow POLICY_KEYPAIR signer {}",
                vault.policy_account, signer_pubkey
            )
            .into());
        }
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let deposit = chain_position_for_reserve(preview, deposit_reserve)?;
    if !deposit.obligation_exists {
        return Err(format!(
            "deposit obligation {} is missing for reserve {}; run the missing-obligation setup transaction before policy deposit",
            deposit.obligation, deposit.reserve
        )
        .into());
    }
    let vault_liquidity_ata =
        derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let reserve_refresh_instruction = kamino_refresh_reserve_instruction(deposit)?;
    let farm_init_instruction =
        kamino_init_obligation_collateral_farm_instruction(payer_pubkey, vault_pubkey, deposit)?;
    let refresh_instruction = kamino_refresh_obligation_instruction(deposit)?;
    let deposit_instruction = kamino_deposit_to_obligation_instruction(
        vault_pubkey,
        deposit,
        vault_liquidity_ata,
        amount,
    )?;
    let instruction_constraint_indexes =
        initial_deposit_instruction_constraint_indexes(policy_preflight)?;
    let policy_constraint_validation = policy_preflight.map(|policy_preflight| {
        let route = [(KAMINO_DEPOSIT_ROUTE_STEP, &deposit_instruction)];
        validate_route_policy_constraints(
            &policy_preflight.decoded,
            &instruction_constraint_indexes,
            &route,
        )
    });
    if let Some(validation) = policy_constraint_validation.as_ref() {
        if !validation.matches {
            return Err(format!(
                "decoded policy account constraints do not match built initial reserve deposit: {}",
                validation.failures.join("; ")
            )
            .into());
        }
    }

    let deposit_instruction_program = deposit_instruction.program_id.to_string();
    let deposit_instruction_discriminator = deposit_instruction.data[..8].to_vec();
    let setup_instruction_program = farm_init_instruction
        .as_ref()
        .map(|instruction| instruction.program_id.to_string());
    let setup_instruction_discriminator = farm_init_instruction
        .as_ref()
        .map(|instruction| instruction.data[..8].to_vec());
    let has_farm_init = farm_init_instruction.is_some();
    let mut pre_instructions = vec![reserve_refresh_instruction];
    if let Some(farm_init_instruction) = farm_init_instruction {
        pre_instructions.push(farm_init_instruction);
    }
    pre_instructions.push(refresh_instruction);

    let mut transaction_accounts = Vec::new();
    let deposit_compiled =
        compile_squads_inner_instruction(&mut transaction_accounts, deposit_instruction);
    let compiled_instructions = vec![deposit_compiled];
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        compiled_instructions.clone(),
        instruction_constraint_indexes.clone(),
        transaction_accounts.clone(),
    );

    Ok(InitialDepositPolicyPlan {
        pre_instructions,
        instruction: outer_instruction.clone(),
        preview: InitialDepositPolicyPreview {
            policy_account: policy_account.to_string(),
            signer: signer_pubkey.to_string(),
            account_index,
            instruction_constraint_indexes,
            policy_constraint_validation,
            setup_instruction_program,
            setup_instruction_discriminator,
            route_steps: if has_farm_init {
                vec![
                    KAMINO_INIT_OBLIGATION_FARM_ROUTE_STEP,
                    KAMINO_DEPOSIT_ROUTE_STEP,
                ]
            } else {
                vec![KAMINO_DEPOSIT_ROUTE_STEP]
            },
            inner_instruction_count: compiled_instructions.len(),
            transaction_account_count: transaction_accounts.len(),
            outer_account_count: outer_instruction.accounts.len(),
            deposit_instruction_program,
            deposit_instruction_discriminator,
        },
    })
}

fn build_full_main_usdc_withdraw_policy_plan(
    vault: &SelectedVault,
    preview: &ChainReconcilePreview,
    policy_preflight: Option<&PolicyAccountPreflight>,
    signer_pubkey: Pubkey,
    account_index: u8,
    withdraw_reserve: &str,
) -> Result<FullWithdrawPolicyPlan, Box<dyn Error>> {
    let policy_account = Pubkey::from_str(&vault.policy_account)?;
    if let Some(policy_preflight) = policy_preflight {
        if !policy_preflight
            .decoded
            .delegated_signers
            .iter()
            .any(|signer| signer == &signer_pubkey.to_string())
        {
            return Err(format!(
                "decoded policy account {} does not allow POLICY_KEYPAIR signer {}",
                vault.policy_account, signer_pubkey
            )
            .into());
        }
    }
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let withdraw = chain_position_for_reserve(preview, withdraw_reserve)?;
    if withdraw.amount_raw == 0 {
        return Err(format!(
            "withdraw obligation account {} has zero deposited amount for reserve {}",
            withdraw.obligation, withdraw.reserve
        )
        .into());
    }
    let vault_liquidity_ata =
        derive_associated_token_address(&vault_pubkey, &USDC_MINT, &spl_token::ID);
    let reserve_refresh_instruction = kamino_refresh_reserve_instruction(withdraw)?;
    let refresh_instruction = kamino_refresh_obligation_instruction(withdraw)?;
    let withdraw_instruction = kamino_withdraw_instruction(
        vault_pubkey,
        withdraw,
        vault_liquidity_ata,
        withdraw.amount_raw,
    )?;
    let instruction_constraint_indexes =
        full_withdraw_instruction_constraint_indexes(policy_preflight)?;
    let policy_constraint_validation = policy_preflight.map(|policy_preflight| {
        validate_route_policy_constraints(
            &policy_preflight.decoded,
            &instruction_constraint_indexes,
            &[(KAMINO_WITHDRAW_ROUTE_STEP, &withdraw_instruction)],
        )
    });
    if let Some(validation) = policy_constraint_validation.as_ref() {
        if !validation.matches {
            return Err(format!(
                "decoded policy account constraints do not match built full reserve withdraw: {}",
                validation.failures.join("; ")
            )
            .into());
        }
    }

    let withdraw_instruction_program = withdraw_instruction.program_id.to_string();
    let withdraw_instruction_discriminator = withdraw_instruction.data[..8].to_vec();
    let mut transaction_accounts = Vec::new();
    let withdraw_compiled =
        compile_squads_inner_instruction(&mut transaction_accounts, withdraw_instruction);
    let compiled_instructions = vec![withdraw_compiled];
    let outer_instruction = execute_program_interaction_policy_instruction(
        policy_account,
        signer_pubkey,
        account_index,
        compiled_instructions.clone(),
        instruction_constraint_indexes.clone(),
        transaction_accounts.clone(),
    );

    Ok(FullWithdrawPolicyPlan {
        pre_instructions: vec![reserve_refresh_instruction, refresh_instruction],
        instruction: outer_instruction.clone(),
        preview: FullWithdrawPolicyPreview {
            policy_account: policy_account.to_string(),
            signer: signer_pubkey.to_string(),
            account_index,
            instruction_constraint_indexes,
            policy_constraint_validation,
            route_steps: vec![KAMINO_WITHDRAW_ROUTE_STEP],
            inner_instruction_count: compiled_instructions.len(),
            transaction_account_count: transaction_accounts.len(),
            outer_account_count: outer_instruction.accounts.len(),
            withdraw_instruction_program,
            withdraw_instruction_discriminator,
        },
    })
}

async fn execute_prepared_same_mint_route(
    client: &NeonSqlClient,
    options: &CliOptions,
    vault: &SelectedVault,
    decision: &PreparedSameMintDecision,
    route_execution: &RouteExecutionPlan,
) -> Result<RouteExecutionSubmitResult, Box<dyn Error>> {
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.to_owned(), CommitmentConfig::confirmed());
    let signer = policy_keypair_from_env()?;
    let admin_fee_payer = if options.optimization_cycle {
        None
    } else {
        Some(solana_testing_keypair_from_env()?)
    };
    let fee_payer: &dyn Signer = admin_fee_payer
        .as_ref()
        .map(|keypair| keypair as &dyn Signer)
        .unwrap_or(&signer);
    let expected_signer = Pubkey::from_str(&route_execution.preview.signer)?;
    if signer.pubkey() != expected_signer {
        return Err(format!(
            "POLICY_KEYPAIR pubkey {} does not match delegated signer {}",
            signer.pubkey(),
            expected_signer
        )
        .into());
    }
    let expected_fee_payer = Pubkey::from_str(&route_execution.preview.fee_payer)?;
    if fee_payer.pubkey() != expected_fee_payer {
        return Err(format!(
            "route fee payer {} does not match prepared route fee payer {}",
            fee_payer.pubkey(),
            expected_fee_payer
        )
        .into());
    }
    let lookup_table_scope = same_mint_route_lookup_table_scope_for_reserves(
        vault,
        &decision.source_reserve,
        &decision.target_reserve,
    );
    let lookup_table_coverage = route_lookup_table_reuse_coverage(
        client,
        &rpc,
        options,
        &lookup_table_scope,
        fee_payer.pubkey(),
        signer.pubkey(),
        route_execution,
    )
    .await?;
    let lookup_table_provisioning =
        lookup_table_coverage.reuse_only_json(options, fee_payer.pubkey());
    ensure_route_lookup_table_coverage(
        &lookup_table_coverage.scope,
        &lookup_table_coverage.missing_addresses,
    )?;
    let lookup_table_accounts = lookup_table_coverage.lookup_table_accounts;
    let mut transaction_instructions = route_execution.pre_instructions.clone();
    transaction_instructions.extend(route_execution.instructions.iter().cloned());
    guard_lookup_table_mutations(
        &transaction_instructions,
        AltInstructionMode::RejectProvisioning,
        "route execution",
    )?;
    let transaction_signers = same_mint_route_signers(fee_payer, &signer);

    let blockhash = rpc.get_latest_blockhash()?;
    let transaction = compile_versioned_transaction(
        fee_payer.pubkey(),
        &transaction_instructions,
        &lookup_table_accounts,
        blockhash,
        &transaction_signers,
    )?;
    let transaction_packet = transaction_packet_summary(&transaction, &lookup_table_accounts)?;
    if !transaction_packet.fits_packet_data_size {
        return Err(format!(
            "route transaction is too large for one packet: {} > {} bytes",
            transaction_packet.packet_size_bytes, transaction_packet.packet_data_size_bytes
        )
        .into());
    }

    client
        .advance_decision(decision.id, DecisionAdvance::StartSimulation)
        .await?;
    let simulation = rpc.simulate_transaction(&transaction)?;
    if let Some(error) = simulation.value.err.as_ref() {
        return Err(format!(
            "route simulation failed: {error:?}; logs: {}",
            simulation.value.logs.as_deref().unwrap_or(&[]).join(" | ")
        )
        .into());
    }
    client
        .advance_decision(decision.id, DecisionAdvance::SimulationReady)
        .await?;

    let submitted_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = rpc.send_and_confirm_transaction(&transaction)?;
    let confirmed_slot = i64::try_from(rpc.get_slot()?)?;
    let signature = signature.to_string();
    client
        .advance_decision(
            decision.id,
            DecisionAdvance::Submit {
                signature: signature.clone(),
                slot: Some(submitted_slot),
            },
        )
        .await?;
    client
        .advance_decision(decision.id, DecisionAdvance::StartConfirmation)
        .await?;
    let post_reconcile_reserves = vec![
        decision.source_reserve.clone(),
        decision.target_reserve.clone(),
    ];
    let post_reconcile_preview =
        load_chain_reconcile_preview(&options.rpc_url, vault, &post_reconcile_reserves)?;
    let post_reconcile_state = chain_preview_reconciled_state(&post_reconcile_preview)?;
    ensure_post_confirm_chain_reconcile_state(decision, &post_reconcile_state)?;
    let post_snapshot = client
        .reconcile_vault(decision.vault_id, post_reconcile_state)
        .await?;
    let confirmed = client
        .confirm_same_mint_rebalance(ConfirmSameMintRebalanceInput {
            decision_id: decision.id,
            signature: signature.clone(),
            submitted_slot: Some(submitted_slot),
            confirmed_slot,
            observed_at: Some(Utc::now()),
            post_snapshot_id: Some(post_snapshot.id),
        })
        .await?;

    Ok(RouteExecutionSubmitResult {
        signature,
        submitted_slot,
        confirmed_slot,
        simulation_units_consumed: simulation.value.units_consumed,
        transaction_packet,
        lookup_table_provisioning,
        confirmed,
    })
}

fn validate_route_policy_constraints(
    decoded: &DecodedPolicyAccount,
    instruction_constraint_indexes: &[u8],
    route: &[(&'static str, &Instruction)],
) -> PolicyConstraintValidation {
    let mut failures = Vec::new();
    if instruction_constraint_indexes.len() != route.len() {
        failures.push(format!(
            "expected {} instruction constraint indexes, got {}",
            route.len(),
            instruction_constraint_indexes.len()
        ));
    }

    for (position, (route_step, instruction)) in route.iter().enumerate() {
        let Some(index) = instruction_constraint_indexes.get(position).copied() else {
            continue;
        };
        let Some(constraint) = decoded.constraints.get(index as usize) else {
            failures.push(format!(
                "{route_step} uses missing policy instruction constraint index {index}"
            ));
            continue;
        };
        failures.extend(validate_instruction_against_policy_constraint(
            route_step,
            constraint,
            instruction,
        ));
    }

    PolicyConstraintValidation {
        matches: failures.is_empty(),
        failures,
    }
}

fn validate_instruction_against_policy_constraint(
    route_step: &str,
    constraint: &PolicyInstructionConstraint,
    instruction: &Instruction,
) -> Vec<String> {
    let mut failures = Vec::new();
    if instruction.program_id != constraint.program_id {
        failures.push(format!(
            "{route_step} program id {} does not match policy program id {}",
            instruction.program_id, constraint.program_id
        ));
    }

    for account_constraint in &constraint.account_constraints {
        let Some(account_meta) = instruction
            .accounts
            .get(account_constraint.account_index as usize)
        else {
            failures.push(format!(
                "{route_step} policy account index {} is out of bounds for built instruction with {} accounts",
                account_constraint.account_index,
                instruction.accounts.len()
            ));
            continue;
        };
        if !account_constraint.pubkeys.is_empty()
            && !account_constraint.pubkeys.contains(&account_meta.pubkey)
        {
            failures.push(format!(
                "{route_step} policy account index {} expects one of [{}], built instruction has {}",
                account_constraint.account_index,
                account_constraint
                    .pubkeys
                    .iter()
                    .map(ToString::to_string)
                    .collect::<Vec<_>>()
                    .join(", "),
                account_meta.pubkey
            ));
        }
    }

    for data_constraint in &constraint.data_constraints {
        if let Err(reason) = policy_data_constraint_matches(data_constraint, &instruction.data) {
            failures.push(format!("{route_step} data constraint mismatch: {reason}"));
        }
    }

    failures
}

fn policy_data_constraint_matches(
    constraint: &PolicyDataConstraint,
    data: &[u8],
) -> Result<(), String> {
    let offset = usize::try_from(constraint.data_offset)
        .map_err(|_| format!("offset {} does not fit usize", constraint.data_offset))?;
    let passed = match &constraint.data_value {
        PolicyDataValue::U8(expected) => compare_policy_values(
            *data
                .get(offset)
                .ok_or_else(|| format!("data too short for u8 at offset {offset}"))?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U16Le(expected) => compare_policy_values(
            read_le_array::<2>(data, offset).map(u16::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U32Le(expected) => compare_policy_values(
            read_le_array::<4>(data, offset).map(u32::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U64Le(expected) => compare_policy_values(
            read_le_array::<8>(data, offset).map(u64::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U128Le(expected) => compare_policy_values(
            read_le_array::<16>(data, offset).map(u128::from_le_bytes)?,
            *expected,
            constraint.operator,
        ),
        PolicyDataValue::U8Slice(expected) => {
            let actual = data
                .get(offset..offset + expected.len())
                .ok_or_else(|| format!("data too short for byte slice at offset {offset}"))?;
            match constraint.operator {
                PolicyDataOperator::Equals => actual == expected.as_slice(),
                PolicyDataOperator::NotEquals => actual != expected.as_slice(),
                other => {
                    return Err(format!(
                        "unsupported byte-slice operator {}",
                        other.as_str()
                    ))
                }
            }
        }
    };

    if passed {
        Ok(())
    } else {
        Err(format!(
            "operator {} failed at offset {}",
            constraint.operator.as_str(),
            constraint.data_offset
        ))
    }
}

fn read_le_array<const N: usize>(data: &[u8], offset: usize) -> Result<[u8; N], String> {
    data.get(offset..offset + N)
        .ok_or_else(|| format!("data too short for {N} bytes at offset {offset}"))?
        .try_into()
        .map_err(|_| format!("failed to read {N} bytes at offset {offset}"))
}

fn compare_policy_values<T: PartialOrd + PartialEq>(
    actual: T,
    expected: T,
    operator: PolicyDataOperator,
) -> bool {
    match operator {
        PolicyDataOperator::Equals => actual == expected,
        PolicyDataOperator::NotEquals => actual != expected,
        PolicyDataOperator::GreaterThan => actual > expected,
        PolicyDataOperator::GreaterThanOrEqualTo => actual >= expected,
        PolicyDataOperator::LessThan => actual < expected,
        PolicyDataOperator::LessThanOrEqualTo => actual <= expected,
    }
}

fn kamino_withdraw_instruction(
    vault: Pubkey,
    source: &ChainPositionSummary,
    vault_liquidity_ata: Pubkey,
    amount: u64,
) -> Result<Instruction, Box<dyn Error>> {
    let reserve = Pubkey::from_str(&source.reserve)?;
    let market = Pubkey::from_str(&source.market)?;
    let liquidity_mint = Pubkey::from_str(&source.liquidity_mint)?;
    let collateral_mint = Pubkey::from_str(&source.collateral_mint)?;
    let reserve_liquidity_supply = Pubkey::from_str(&source.reserve_liquidity_supply)?;
    let reserve_collateral_supply = Pubkey::from_str(&source.reserve_collateral_supply)?;
    let liquidity_token_program = Pubkey::from_str(&source.liquidity_token_program)?;
    let (obligation_farm_user_state, reserve_farm_state) = collateral_farm_accounts(source)?;
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &market);
    let (obligation_account, _) = obligation(
        &KLEND_PROGRAM_ID,
        0,
        0,
        &vault,
        &market,
        &Pubkey::default(),
        &Pubkey::default(),
    );

    Ok(
        withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
                owner: vault,
                obligation: obligation_account,
                lending_market: market,
                lending_market_authority,
                withdraw_reserve: reserve,
                reserve_liquidity_mint: liquidity_mint,
                reserve_source_collateral: reserve_collateral_supply,
                reserve_collateral_mint: collateral_mint,
                reserve_liquidity_supply,
                user_destination_liquidity: vault_liquidity_ata,
                placeholder_user_destination_collateral: None,
                liquidity_token_program,
                obligation_farm_user_state,
                reserve_farm_state,
            },
            amount,
        ),
    )
}

fn kamino_deposit_to_obligation_instruction(
    vault: Pubkey,
    target: &ChainPositionSummary,
    vault_liquidity_ata: Pubkey,
    amount: u64,
) -> Result<Instruction, Box<dyn Error>> {
    let reserve = Pubkey::from_str(&target.reserve)?;
    let market = Pubkey::from_str(&target.market)?;
    let liquidity_mint = Pubkey::from_str(&target.liquidity_mint)?;
    let collateral_mint = Pubkey::from_str(&target.collateral_mint)?;
    let reserve_liquidity_supply = Pubkey::from_str(&target.reserve_liquidity_supply)?;
    let reserve_collateral_supply = Pubkey::from_str(&target.reserve_collateral_supply)?;
    let liquidity_token_program = Pubkey::from_str(&target.liquidity_token_program)?;
    let (obligation_farm_user_state, reserve_farm_state) = collateral_farm_accounts(target)?;
    let (lending_market_authority, _) = lending_market_authority(&KLEND_PROGRAM_ID, &market);
    let (obligation_account, _) = obligation(
        &KLEND_PROGRAM_ID,
        0,
        0,
        &vault,
        &market,
        &Pubkey::default(),
        &Pubkey::default(),
    );

    Ok(deposit_reserve_liquidity_and_obligation_collateral_v2(
        DepositReserveLiquidityAndObligationCollateralV2Accounts {
            owner: vault,
            obligation: obligation_account,
            lending_market: market,
            lending_market_authority,
            reserve,
            reserve_liquidity_mint: liquidity_mint,
            reserve_liquidity_supply,
            reserve_collateral_mint: collateral_mint,
            reserve_destination_deposit_collateral: reserve_collateral_supply,
            user_source_liquidity: vault_liquidity_ata,
            placeholder_user_destination_collateral: None,
            liquidity_token_program,
            obligation_farm_user_state,
            reserve_farm_state,
        },
        amount,
    ))
}

fn kamino_refresh_reserve_instruction(
    position: &ChainPositionSummary,
) -> Result<Instruction, Box<dyn Error>> {
    Ok(refresh_reserve(RefreshReserveAccounts {
        reserve: Pubkey::from_str(&position.reserve)?,
        lending_market: Pubkey::from_str(&position.market)?,
        pyth_oracle: optional_pubkey_from_string(position.pyth_oracle.as_deref())?,
        switchboard_price_oracle: optional_pubkey_from_string(
            position.switchboard_price_oracle.as_deref(),
        )?,
        switchboard_twap_oracle: optional_pubkey_from_string(
            position.switchboard_twap_oracle.as_deref(),
        )?,
        scope_prices: optional_pubkey_from_string(position.scope_prices.as_deref())?,
    }))
}

fn kamino_init_obligation_collateral_farm_instruction(
    payer: Pubkey,
    owner: Pubkey,
    position: &ChainPositionSummary,
) -> Result<Option<Instruction>, Box<dyn Error>> {
    let Some(reserve_farm_state) = &position.collateral_farm else {
        return Ok(None);
    };
    if position.collateral_farm_user_state_exists {
        return Ok(None);
    }
    let obligation_farm = position
        .collateral_farm_user_state
        .as_deref()
        .ok_or("collateral farm state was present without a derived farm user state")?;
    let lending_market = Pubkey::from_str(&position.market)?;
    let reserve_farm_state = Pubkey::from_str(reserve_farm_state)?;
    let obligation = derive_kamino_vanilla_obligation(owner, lending_market);
    if Pubkey::from_str(&position.obligation)? != obligation {
        return Err(format!(
            "chain preview obligation {} does not match derived vanilla obligation {}",
            position.obligation, obligation
        )
        .into());
    }
    let derived_obligation_farm =
        derive_kamino_obligation_farm_user_state(reserve_farm_state, obligation);
    if Pubkey::from_str(obligation_farm)? != derived_obligation_farm {
        return Err(format!(
            "chain preview collateral farm user state {obligation_farm} does not match derived farm user state {derived_obligation_farm}"
        )
        .into());
    }

    Ok(Some(kamino_init_obligation_farm_instruction(
        KaminoInitObligationFarm {
            payer,
            owner,
            lending_market,
            reserve: Pubkey::from_str(&position.reserve)?,
            reserve_farm_state,
        },
    )))
}

fn optional_pubkey_from_string(value: Option<&str>) -> Result<Option<Pubkey>, Box<dyn Error>> {
    value
        .map(Pubkey::from_str)
        .transpose()
        .map_err(|error| error.into())
}

fn collateral_farm_accounts(
    position: &ChainPositionSummary,
) -> Result<(Option<Pubkey>, Option<Pubkey>), Box<dyn Error>> {
    let Some(collateral_farm) = &position.collateral_farm else {
        return Ok((None, None));
    };
    let reserve_farm_state = Pubkey::from_str(collateral_farm)?;
    let obligation_account = Pubkey::from_str(&position.obligation)?;
    let (obligation_farm_user_state, _) =
        farms_user_state(&reserve_farm_state, &obligation_account);
    Ok((Some(obligation_farm_user_state), Some(reserve_farm_state)))
}

fn kamino_refresh_obligation_instruction(
    position: &ChainPositionSummary,
) -> Result<Instruction, Box<dyn Error>> {
    let remaining_reserves = position
        .obligation_deposit_reserves
        .iter()
        .chain(position.obligation_borrow_reserves.iter())
        .map(String::as_str)
        .collect::<Vec<_>>();

    kamino_refresh_obligation_for_reserves_instruction(position, &remaining_reserves)
}

fn kamino_refresh_obligation_for_reserves_instruction(
    position: &ChainPositionSummary,
    reserves: &[&str],
) -> Result<Instruction, Box<dyn Error>> {
    let lending_market = Pubkey::from_str(&position.market)?;
    let obligation = Pubkey::from_str(&position.obligation)?;
    let remaining_accounts = reserves
        .iter()
        .map(|reserve| {
            Pubkey::from_str(reserve)
                .map(|pubkey| AccountMeta::new(pubkey, false))
                .map_err(|error| format!("invalid obligation reserve {reserve}: {error}"))
        })
        .collect::<Result<Vec<_>, _>>()?;

    Ok(refresh_obligation(
        RefreshObligationAccounts {
            lending_market,
            obligation,
        },
        remaining_accounts,
    ))
}

fn kamino_init_obligation_instruction(
    vault: Pubkey,
    target: &ChainPositionSummary,
) -> Result<Instruction, Box<dyn Error>> {
    let market = Pubkey::from_str(&target.market)?;
    let seed1 = Pubkey::default();
    let seed2 = Pubkey::default();
    let (obligation_account, _) =
        obligation(&KLEND_PROGRAM_ID, 0, 0, &vault, &market, &seed1, &seed2);
    let (owner_user_metadata, _) = user_metadata(&KLEND_PROGRAM_ID, &vault);

    Ok(init_obligation(
        InitObligationAccounts {
            obligation_owner: vault,
            fee_payer: vault,
            obligation: obligation_account,
            lending_market: market,
            seed1_account: seed1,
            seed2_account: seed2,
            owner_user_metadata,
        },
        InitObligationArgs { tag: 0, id: 0 },
    ))
}

fn route_instruction_constraint_indexes(
    vault: &SelectedVault,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<Vec<u8>, Box<dyn Error>> {
    if let Some(policy_preflight) = policy_preflight {
        return decoded_route_instruction_constraint_indexes(&policy_preflight.decoded);
    }

    let _ = vault;
    Err("same-mint route requires decoded policy account indexes".into())
}

fn decoded_route_instruction_constraint_indexes(
    decoded: &DecodedPolicyAccount,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let withdraw =
        decoded_instruction_index(decoded, KAMINO_WITHDRAW_ROUTE_STEP, "Kamino withdraw route")?;
    let deposit =
        decoded_instruction_index(decoded, KAMINO_DEPOSIT_ROUTE_STEP, "Kamino deposit route")?;
    let mut indexes = Vec::new();
    indexes.push(u8::try_from(withdraw)?);
    indexes.push(u8::try_from(deposit)?);
    Ok(indexes)
}

fn resolve_init_obligation_policy(
    rpc: Option<&RpcClient>,
    vault: &SelectedVault,
    target: &ChainPositionSummary,
    route_policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<(Pubkey, u8), Box<dyn Error>> {
    if let Some(preflight) = route_policy_preflight {
        if let Ok(index) = init_obligation_instruction_constraint_index(Some(preflight), target) {
            return Ok((Pubkey::from_str(&preflight.policy_account)?, index));
        }
    }

    let setup_policy_account = vault.setup_policy_account.as_deref().ok_or_else(|| {
        format!(
            "target obligation {} is missing, active policy {} has no authorized init_obligation path for target market {}, and no setup_policy_id is recorded for vault {}",
            target.obligation, vault.policy_account, target.market, vault.id
        )
    })?;
    let rpc = rpc.ok_or(
        "setup policy account decode requires an RPC client when target init is not in route policy",
    )?;
    let setup_policy = Pubkey::from_str(setup_policy_account)?;
    let account = rpc.get_account(&setup_policy)?;
    let decoded = decode_squads_policy_account(&account.data).map_err(|error| {
        format!("failed to decode setup policy account {setup_policy}: {error}")
    })?;
    let setup_preflight = PolicyAccountPreflight {
        policy_account: setup_policy_account.to_owned(),
        source_market: target.market.clone(),
        target_market: target.market.clone(),
        decoded,
    };
    let index = init_obligation_instruction_constraint_index(Some(&setup_preflight), target)?;
    Ok((setup_policy, index))
}

fn init_obligation_instruction_constraint_index(
    policy_preflight: Option<&PolicyAccountPreflight>,
    target: &ChainPositionSummary,
) -> Result<u8, Box<dyn Error>> {
    let Some(policy_preflight) = policy_preflight else {
        return Err("init obligation setup requires decoded policy account indexes".into());
    };
    let index = policy_preflight
        .decoded
        .instructions
        .iter()
        .position(|instruction| {
            instruction.route_step == Some(KAMINO_INIT_OBLIGATION_ROUTE_STEP)
                && instruction
                    .markets
                    .iter()
                    .any(|market| market == &target.market)
        })
        .ok_or_else(|| {
            format!(
                "decoded policy account has no market-scoped init_obligation constraint for target market {}",
                target.market
            )
        })?;
    if index >= policy_preflight.decoded.instruction_count {
        return Err(format!(
            "decoded init_obligation index {index} exceeds policy instruction count {}",
            policy_preflight.decoded.instruction_count
        )
        .into());
    }
    Ok(u8::try_from(index)?)
}

fn initial_deposit_instruction_constraint_indexes(
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let Some(policy_preflight) = policy_preflight else {
        return Err("initial deposit requires decoded policy account indexes".into());
    };
    let decoded = &policy_preflight.decoded;
    let deposit =
        decoded_instruction_index(decoded, KAMINO_DEPOSIT_ROUTE_STEP, "Kamino deposit route")?;
    Ok(vec![u8::try_from(deposit)?])
}

fn full_withdraw_instruction_constraint_indexes(
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let Some(policy_preflight) = policy_preflight else {
        return Err("full withdraw requires decoded policy account indexes".into());
    };
    let decoded = &policy_preflight.decoded;
    let withdraw =
        decoded_instruction_index(decoded, KAMINO_WITHDRAW_ROUTE_STEP, "Kamino withdraw route")?;
    Ok(vec![u8::try_from(withdraw)?])
}

fn decoded_instruction_index(
    decoded: &DecodedPolicyAccount,
    route_step: &'static str,
    label: &'static str,
) -> Result<usize, Box<dyn Error>> {
    let index = decoded
        .instructions
        .iter()
        .position(|instruction| instruction.route_step == Some(route_step))
        .ok_or_else(|| format!("decoded policy account has no {label} constraint"))?;
    if index >= decoded.instruction_count {
        return Err(format!(
            "decoded {label} index {index} exceeds policy instruction count {}",
            decoded.instruction_count
        )
        .into());
    }
    Ok(index)
}

fn preview_position_summaries(preview: &ChainReconcilePreview) -> Vec<PositionSummary> {
    preview
        .positions
        .iter()
        .map(|position| PositionSummary {
            reserve: position.reserve.clone(),
            liquidity_mint: position.liquidity_mint.clone(),
            amount_raw: i64::try_from(position.amount_raw).unwrap_or(i64::MAX),
            has_value: position.amount_raw > 0,
            snapshot_id: SnapshotId(0),
            supply_apy_bps: None,
            planning_metadata: json!({
                "source": "chain_reconcile_preview",
                "amount_semantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
                "source_collateral_amount_raw": position.amount_raw.to_string(),
                "redeemable_source_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                "redeemable_liquidity_amount_raw": position.redeemable_liquidity_amount_raw.to_string(),
                "obligation": position.obligation,
                "obligation_exists": position.obligation_exists,
                "vault_liquidity_ata": position.vault_liquidity_ata,
                "vault_liquidity_token_account_exists": position.vault_liquidity_token_account_exists,
                "idle_vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
                "vault_liquidity_amount_raw": position.vault_liquidity_amount_raw.to_string(),
            }),
        })
        .collect()
}

#[derive(Debug)]
struct KaminoReserveSummary {
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
    collateral_total_supply: u64,
    total_liquidity_scaled: BigUint,
}

fn load_kamino_reserve_summary(
    rpc: &RpcClient,
    reserve: &Pubkey,
) -> Result<KaminoReserveSummary, Box<dyn Error>> {
    let account = rpc.get_account(reserve)?;
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "reserve {reserve} is owned by {}, expected live Kamino lend program {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let reserve_state = from_account_data::<Reserve>(&account.data)?;
    Ok(KaminoReserveSummary {
        market: reserve_state.lending_market,
        liquidity_mint: reserve_state.liquidity.mint_pubkey,
        liquidity_token_program: reserve_state.liquidity.token_program,
        liquidity_supply: reserve_state.liquidity.supply_vault,
        collateral_mint: reserve_state.collateral.mint_pubkey,
        collateral_supply: reserve_state.collateral.supply_vault,
        collateral_total_supply: reserve_state.collateral.mint_total_supply,
        total_liquidity_scaled: reserve_total_liquidity_scaled(&reserve_state)?,
        collateral_farm: if reserve_state.farm_collateral == Pubkey::default() {
            None
        } else {
            Some(reserve_state.farm_collateral)
        },
        pyth_oracle: non_default_pubkey(reserve_state.config.token_info.pyth_configuration.price),
        switchboard_price_oracle: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .switchboard_configuration
                .price_aggregator,
        ),
        switchboard_twap_oracle: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .switchboard_configuration
                .twap_aggregator,
        ),
        scope_prices: non_default_pubkey(
            reserve_state
                .config
                .token_info
                .scope_configuration
                .price_feed,
        ),
    })
}

fn reserve_total_liquidity_scaled(reserve: &Reserve) -> Result<BigUint, Box<dyn Error>> {
    let scale = BigUint::from(1_u128 << 60);
    let mut total = BigUint::from(reserve.liquidity.total_available_amount) * &scale;
    total += BigUint::from(u128::from(reserve.liquidity.borrowed_amount_sf));
    subtract_scaled_fraction(
        &mut total,
        u128::from(reserve.liquidity.accumulated_protocol_fees_sf),
        "accumulated protocol fees",
    )?;
    subtract_scaled_fraction(
        &mut total,
        u128::from(reserve.liquidity.accumulated_referrer_fees_sf),
        "accumulated referrer fees",
    )?;
    subtract_scaled_fraction(
        &mut total,
        u128::from(reserve.liquidity.pending_referrer_fees_sf),
        "pending referrer fees",
    )?;
    Ok(total)
}

fn subtract_scaled_fraction(
    total: &mut BigUint,
    amount: u128,
    label: &'static str,
) -> Result<(), Box<dyn Error>> {
    let amount = BigUint::from(amount);
    if (&*total) < &amount {
        return Err(format!("reserve total liquidity underflow subtracting {label}").into());
    }
    *total -= amount;
    Ok(())
}

fn collateral_to_redeemable_liquidity_amount(
    collateral_total_supply: u64,
    total_liquidity_scaled: &BigUint,
    collateral_amount: u64,
) -> Result<u64, Box<dyn Error>> {
    if collateral_amount == 0 {
        return Ok(0);
    }
    if collateral_total_supply == 0 || total_liquidity_scaled.is_zero() {
        return Ok(collateral_amount);
    }

    let scale = BigUint::from(1_u128 << 60);
    let numerator = BigUint::from(collateral_amount) * total_liquidity_scaled;
    let denominator = BigUint::from(collateral_total_supply) * scale;
    (numerator / denominator)
        .to_u64()
        .ok_or_else(|| "redeemable liquidity amount does not fit u64".into())
}

fn non_default_pubkey(pubkey: Pubkey) -> Option<Pubkey> {
    if pubkey == Pubkey::default() {
        None
    } else {
        Some(pubkey)
    }
}

struct KaminoObligationSummary {
    exists: bool,
    reserve_deposited_amount_raw: u64,
    deposit_reserves: Vec<String>,
    borrow_reserves: Vec<String>,
}

fn load_kamino_obligation_summary(
    rpc: &RpcClient,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
) -> Result<KaminoObligationSummary, Box<dyn Error>> {
    let response =
        rpc.get_account_with_commitment(obligation_account, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(KaminoObligationSummary {
            exists: false,
            reserve_deposited_amount_raw: 0,
            deposit_reserves: Vec::new(),
            borrow_reserves: Vec::new(),
        });
    };
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "obligation account {obligation_account} is owned by {}, expected {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let obligation_state = from_account_data::<Obligation>(&account.data)?;
    if obligation_state.owner != *expected_owner {
        return Err(format!(
            "obligation account {obligation_account} owner {} does not match vault {}",
            obligation_state.owner, expected_owner
        )
        .into());
    }
    if obligation_state.lending_market != *expected_market {
        return Err(format!(
            "obligation account {obligation_account} market {} does not match reserve market {}",
            obligation_state.lending_market, expected_market
        )
        .into());
    }

    let amount = obligation_state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == *reserve)
        .map(|deposit| deposit.deposited_amount)
        .unwrap_or_default();
    let deposit_reserves = obligation_state
        .deposits
        .iter()
        .filter(|deposit| deposit.deposit_reserve != Pubkey::default())
        .map(|deposit| deposit.deposit_reserve.to_string())
        .collect();
    let borrow_reserves = obligation_state
        .borrows
        .iter()
        .filter(|borrow| borrow.borrow_reserve != Pubkey::default())
        .map(|borrow| borrow.borrow_reserve.to_string())
        .collect();

    Ok(KaminoObligationSummary {
        exists: true,
        reserve_deposited_amount_raw: amount,
        deposit_reserves,
        borrow_reserves,
    })
}

fn pubkey_from_account_data(
    data: &[u8],
    offset: usize,
    field: &'static str,
) -> Result<Pubkey, Box<dyn Error>> {
    let bytes = data
        .get(offset..offset + PUBKEY_LEN)
        .ok_or_else(|| format!("account data too short for {field} at offset {offset}"))?;
    Ok(Pubkey::new_from_array(bytes.try_into()?))
}

fn derive_associated_token_address(
    owner: &Pubkey,
    mint: &Pubkey,
    token_program: &Pubkey,
) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn create_associated_token_account_idempotent_instruction(
    funding_address: Pubkey,
    wallet_address: Pubkey,
    token_mint_address: Pubkey,
    token_program_id: Pubkey,
) -> Instruction {
    let associated_account_address =
        derive_associated_token_address(&wallet_address, &token_mint_address, &token_program_id);
    Instruction {
        program_id: ASSOCIATED_TOKEN_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(funding_address, true),
            AccountMeta::new(associated_account_address, false),
            AccountMeta::new_readonly(wallet_address, false),
            AccountMeta::new_readonly(token_mint_address, false),
            AccountMeta::new_readonly(system_program::ID, false),
            AccountMeta::new_readonly(token_program_id, false),
        ],
        data: vec![1],
    }
}

fn load_spl_token_account_amount(
    rpc: &RpcClient,
    token_account: &Pubkey,
    expected_mint: &Pubkey,
) -> Result<(u64, bool), Box<dyn Error>> {
    let response = rpc.get_account_with_commitment(token_account, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok((0, false));
    };
    if account.owner != spl_token::ID {
        return Err(format!(
            "token account {token_account} is owned by {}, expected {}",
            account.owner,
            spl_token::ID
        )
        .into());
    }
    let mint = pubkey_from_account_data(
        &account.data,
        SPL_TOKEN_ACCOUNT_MINT_OFFSET,
        "token account mint",
    )?;
    if mint != *expected_mint {
        return Err(format!(
            "token account {token_account} mint {mint} does not match expected {expected_mint}"
        )
        .into());
    }
    let amount_bytes = account
        .data
        .get(SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET..SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET + 8)
        .ok_or_else(|| {
            format!(
                "token account data too short for amount at offset {SPL_TOKEN_ACCOUNT_AMOUNT_OFFSET}"
            )
        })?;
    Ok((u64::from_le_bytes(amount_bytes.try_into()?), true))
}

fn load_account_proof(rpc: &RpcClient, pubkey: &Pubkey) -> Result<AccountProof, Box<dyn Error>> {
    let response = rpc.get_account_with_commitment(pubkey, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(AccountProof {
            pubkey: pubkey.to_string(),
            exists: false,
            lamports: 0,
            owner: None,
        });
    };
    Ok(AccountProof {
        pubkey: pubkey.to_string(),
        exists: true,
        lamports: account.lamports,
        owner: Some(account.owner.to_string()),
    })
}

fn load_obligation_account_proof(
    rpc: &RpcClient,
    obligation_account: &Pubkey,
    expected_owner: &Pubkey,
    expected_market: &Pubkey,
    reserve: &Pubkey,
) -> Result<ObligationAccountProof, Box<dyn Error>> {
    let response =
        rpc.get_account_with_commitment(obligation_account, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(ObligationAccountProof {
            account: AccountProof {
                pubkey: obligation_account.to_string(),
                exists: false,
                lamports: 0,
                owner: None,
            },
            owner: None,
            lending_market: None,
            active_deposit_count: None,
            active_borrow_count: None,
            reserve_deposited_amount_raw: None,
        });
    };
    if account.owner != KLEND_PROGRAM_ID {
        return Err(format!(
            "obligation account {obligation_account} is owned by {}, expected {}",
            account.owner, KLEND_PROGRAM_ID
        )
        .into());
    }
    let obligation_state = from_account_data::<Obligation>(&account.data)?;
    if obligation_state.owner != *expected_owner {
        return Err(format!(
            "obligation account {obligation_account} owner {} does not match vault {}",
            obligation_state.owner, expected_owner
        )
        .into());
    }
    if obligation_state.lending_market != *expected_market {
        return Err(format!(
            "obligation account {obligation_account} market {} does not match expected {}",
            obligation_state.lending_market, expected_market
        )
        .into());
    }
    let reserve_deposited_amount_raw = obligation_state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == *reserve)
        .map(|deposit| deposit.deposited_amount);
    Ok(ObligationAccountProof {
        account: AccountProof {
            pubkey: obligation_account.to_string(),
            exists: true,
            lamports: account.lamports,
            owner: Some(account.owner.to_string()),
        },
        owner: Some(obligation_state.owner.to_string()),
        lending_market: Some(obligation_state.lending_market.to_string()),
        active_deposit_count: Some(obligation_state.num_deposits()),
        active_borrow_count: Some(obligation_state.num_borrows()),
        reserve_deposited_amount_raw,
    })
}

fn account_exists_with_owner(
    rpc: &RpcClient,
    pubkey: &Pubkey,
    expected_owner: &Pubkey,
) -> Result<bool, Box<dyn Error>> {
    let response = rpc.get_account_with_commitment(pubkey, CommitmentConfig::confirmed())?;
    let Some(account) = response.value else {
        return Ok(false);
    };
    if account.owner != *expected_owner {
        return Err(format!(
            "account {pubkey} is owned by {}, expected {}",
            account.owner, expected_owner
        )
        .into());
    }
    Ok(true)
}

fn dedup_strings_in_place(values: &mut Vec<String>) {
    let mut deduped = Vec::with_capacity(values.len());
    for value in values.drain(..) {
        if !deduped.contains(&value) {
            deduped.push(value);
        }
    }
    *values = deduped;
}

async fn connect(database_url: &str) -> Result<PgPool, loyal_yield_orchestrator::sqlx::Error> {
    let options = PgConnectOptions::from_str(database_url)?.statement_cache_capacity(0);
    PgPoolOptions::new()
        .max_connections(2)
        .connect_with(options)
        .await
}

async fn load_active_vault(
    pool: &PgPool,
    settings: &str,
    vault_index: i16,
) -> Result<Option<SelectedVault>, loyal_yield_orchestrator::sqlx::Error> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id,
            v.settings,
            p.authority,
            p.policy_seed,
            v.vault_index,
            v.vault_pubkey,
            p.policy_account,
            sp.policy_account AS setup_policy_account,
            sp.policy_seed AS setup_policy_seed,
            p.delegated_signers,
            p.threshold,
            p.route_modes,
            p.stable_mints,
            p.kamino_markets,
            p.kamino_liquidity_mints,
            p.swap_lanes
        FROM loyal_yield.managed_vaults v
        JOIN loyal_yield.route_policies p ON p.id = v.active_policy_id
        LEFT JOIN loyal_yield.route_policies sp ON sp.id = v.setup_policy_id
          AND sp.active = true
        WHERE v.settings = $1
          AND v.vault_index = $2
          AND v.active = true
          AND p.active = true
        "#,
    )
    .bind(settings)
    .bind(vault_index)
    .fetch_optional(pool)
    .await?;

    row.map(|row| {
        Ok(SelectedVault {
            id: VaultId(row.try_get::<i64, _>("id")?),
            settings: row.try_get("settings")?,
            authority: row.try_get("authority")?,
            policy_seed: row.try_get("policy_seed")?,
            vault_index: row.try_get("vault_index")?,
            vault_pubkey: row.try_get("vault_pubkey")?,
            policy_account: row.try_get("policy_account")?,
            setup_policy_account: row.try_get("setup_policy_account")?,
            setup_policy_seed: row.try_get("setup_policy_seed")?,
            delegated_signers: row.try_get("delegated_signers")?,
            threshold: row.try_get("threshold")?,
            route_modes: row.try_get("route_modes")?,
            stable_mints: row.try_get("stable_mints")?,
            kamino_markets: row.try_get("kamino_markets")?,
            kamino_liquidity_mints: row.try_get("kamino_liquidity_mints")?,
            swap_lanes: row.try_get("swap_lanes")?,
        })
    })
    .transpose()
}

async fn load_policy_target_vault(
    pool: &PgPool,
    settings: &str,
    vault_index: i16,
    default_authority: Pubkey,
    default_delegated_signer: Pubkey,
) -> Result<Option<SelectedVault>, Box<dyn Error>> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT
            v.id,
            v.settings,
            v.vault_index,
            v.vault_pubkey,
            seed_cursor.max_policy_seed,
            route_template.authority,
            route_template.delegated_signers,
            route_template.threshold,
            route_template.route_modes,
            route_template.stable_mints,
            route_template.kamino_markets,
            route_template.kamino_liquidity_mints,
            route_template.swap_lanes
        FROM loyal_yield.managed_vaults v
        LEFT JOIN LATERAL (
            SELECT max(policy_seed) AS max_policy_seed
            FROM loyal_yield.route_policies
            WHERE settings = v.settings
              AND vault_index = v.vault_index
        ) seed_cursor ON TRUE
        LEFT JOIN LATERAL (
            SELECT
                authority,
                delegated_signers,
                threshold,
                route_modes,
                stable_mints,
                kamino_markets,
                kamino_liquidity_mints,
                swap_lanes
            FROM loyal_yield.route_policies
            WHERE settings = v.settings
              AND vault_index = v.vault_index
              AND $3 = ANY(route_modes)
            ORDER BY active DESC, last_seen_slot DESC, policy_seed DESC, id DESC
            LIMIT 1
        ) route_template ON TRUE
        WHERE v.settings = $1
          AND v.vault_index = $2
        "#,
    )
    .bind(settings)
    .bind(vault_index)
    .bind(SAME_MINT_ROUTE_MODE)
    .fetch_optional(pool)
    .await?;

    let Some(row) = row else {
        return Ok(None);
    };

    let settings: String = row.try_get("settings")?;
    let settings_pubkey = Pubkey::from_str(&settings)?;
    let max_policy_seed: Option<i64> = row.try_get("max_policy_seed")?;
    let policy_seed = max_policy_seed
        .map(|seed| seed.saturating_add(1))
        .unwrap_or(i64::try_from(YIELD_ROUTE_WITHDRAW_ACTION_SEED)?);
    let policy_account = derive_action_account(&settings_pubkey, u64::try_from(policy_seed)?).0;
    let authority = row
        .try_get::<Option<String>, _>("authority")?
        .unwrap_or_else(|| default_authority.to_string());
    let delegated_signers = row
        .try_get::<Option<Vec<String>>, _>("delegated_signers")?
        .unwrap_or_else(|| vec![default_delegated_signer.to_string()]);
    let threshold = row.try_get::<Option<i32>, _>("threshold")?.unwrap_or(1);
    let route_modes = row
        .try_get::<Option<Vec<String>>, _>("route_modes")?
        .unwrap_or_else(|| vec![SAME_MINT_ROUTE_MODE.to_owned()]);
    let stable_mints = row
        .try_get::<Option<Vec<String>>, _>("stable_mints")?
        .unwrap_or_else(|| vec![USDC_MINT.to_string()]);
    let kamino_markets = row
        .try_get::<Option<Vec<String>>, _>("kamino_markets")?
        .unwrap_or_else(|| {
            vec![
                KAMINO_MAIN_MARKET.to_owned(),
                KAMINO_PRIME_MARKET.to_owned(),
            ]
        });
    let kamino_liquidity_mints = row
        .try_get::<Option<Vec<String>>, _>("kamino_liquidity_mints")?
        .unwrap_or_else(|| vec![USDC_MINT.to_string()]);
    let swap_lanes = row
        .try_get::<Option<Value>, _>("swap_lanes")?
        .unwrap_or_else(|| Value::Array(vec![]));

    Ok(Some(SelectedVault {
        id: VaultId(row.try_get::<i64, _>("id")?),
        settings,
        authority,
        policy_seed,
        vault_index: row.try_get("vault_index")?,
        vault_pubkey: row.try_get("vault_pubkey")?,
        policy_account: policy_account.to_string(),
        setup_policy_account: None,
        setup_policy_seed: None,
        delegated_signers,
        threshold,
        route_modes,
        stable_mints,
        kamino_markets,
        kamino_liquidity_mints,
        swap_lanes,
    }))
}

async fn load_active_decision(
    pool: &PgPool,
    vault_id: VaultId,
) -> Result<Option<(i64, String)>, loyal_yield_orchestrator::sqlx::Error> {
    let row = loyal_yield_orchestrator::sqlx::query(
        r#"
        SELECT id, status::text AS status
        FROM loyal_yield.rebalance_decisions
        WHERE vault_id = $1
          AND status = ANY($2::loyal_yield.decision_status[])
        ORDER BY id DESC
        LIMIT 1
        "#,
    )
    .bind(vault_id.as_i64())
    .bind(&["planned", "simulating", "ready", "submitted", "confirming"])
    .fetch_optional(pool)
    .await?;

    row.map(|row| Ok((row.try_get("id")?, row.try_get("status")?)))
        .transpose()
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<CliOptions, String> {
    let mut settings = None;
    let mut vault_index = None;
    let mut direction = Direction::MainToPrime;
    let mut source_reserve = None;
    let mut target_reserve = None;
    let mut update_policy = false;
    let mut update_active_policy = false;
    let mut initial_deposit_reserve = None;
    let mut initial_deposit_amount_raw = None;
    let mut idle_vault_deposit_reserve = None;
    let mut idle_vault_deposit_amount_raw = None;
    let mut full_withdraw_main_usdc = false;
    let mut full_withdraw_reserve = None;
    let mut setup_obligation_reserve = None;
    let mut e2e_deposit_amount_raw = None;
    let mut execute = false;
    let mut optimization_cycle = false;
    let mut reconcile_from_chain = false;
    let mut reconcile_current_positions = false;
    let mut reconcile_reserves = Vec::new();
    let mut seed_from_user_position = false;
    let mut provision_lookup_table = false;
    let mut provision_route_lookup_table = false;
    let mut expected_source_snapshot_id = None;
    let mut expected_liquidity_mint = None;
    let mut expected_amount_raw = None;
    let mut expected_route_amount_semantics = None;
    let mut expected_idle_token_account = None;
    let mut expected_idle_observed_slot = None;
    let mut expected_idle_observed_at = None;
    let mut expected_source_apy_bps = None;
    let mut expected_target_apy_bps = None;
    let mut expected_edge_bps = None;
    let mut rpc_url = env::var("SOLANA_RPC_URL").unwrap_or_else(|_| DEFAULT_SOLANA_RPC_URL.into());
    let mut lookup_tables = Vec::new();
    let mut iter = args.into_iter();

    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--settings" => {
                settings = Some(
                    iter.next()
                        .ok_or("--settings requires a settings public key")?,
                );
            }
            "--vault-index" => {
                let raw = iter.next().ok_or("--vault-index requires a value")?;
                vault_index = Some(
                    raw.parse::<i16>()
                        .map_err(|_| "--vault-index must be an i16")?,
                );
            }
            "--direction" => {
                let raw = iter.next().ok_or("--direction requires a value")?;
                direction = Direction::parse(&raw)
                    .ok_or("--direction must be main-to-prime or prime-to-main")?;
            }
            "--source-reserve" => {
                source_reserve = Some(
                    iter.next()
                        .ok_or("--source-reserve requires a public key")?,
                );
            }
            "--target-reserve" => {
                target_reserve = Some(
                    iter.next()
                        .ok_or("--target-reserve requires a public key")?,
                );
            }
            "--update-policy" => update_policy = true,
            "--update-active-policy" => update_active_policy = true,
            "--full-withdraw-main-usdc" => full_withdraw_main_usdc = true,
            "--full-withdraw-reserve" => {
                let raw = iter
                    .next()
                    .ok_or("--full-withdraw-reserve requires a reserve public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--full-withdraw-reserve must be a public key")?;
                full_withdraw_reserve = Some(raw);
            }
            "--setup-obligation-reserve" => {
                let raw = iter
                    .next()
                    .ok_or("--setup-obligation-reserve requires a reserve public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--setup-obligation-reserve must be a public key")?;
                setup_obligation_reserve = Some(raw);
            }
            "--e2e-main-prime-main" => {
                let raw = iter
                    .next()
                    .ok_or("--e2e-main-prime-main requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--e2e-main-prime-main amount must be a u64")?;
                if amount == 0 {
                    return Err("--e2e-main-prime-main amount must be greater than 0".to_owned());
                }
                e2e_deposit_amount_raw = Some(amount);
            }
            "--deposit-main-usdc" => {
                if initial_deposit_amount_raw.is_some() {
                    return Err("choose only one initial deposit mode".to_owned());
                }
                let raw = iter
                    .next()
                    .ok_or("--deposit-main-usdc requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--deposit-main-usdc amount must be a u64")?;
                if amount == 0 {
                    return Err("--deposit-main-usdc amount must be greater than 0".to_owned());
                }
                initial_deposit_reserve = Some(KAMINO_MAIN_USDC_RESERVE.to_string());
                initial_deposit_amount_raw = Some(amount);
            }
            "--deposit-reserve" => {
                if initial_deposit_amount_raw.is_some() {
                    return Err("choose only one initial deposit mode".to_owned());
                }
                let reserve = iter
                    .next()
                    .ok_or("--deposit-reserve requires a reserve public key")?;
                Pubkey::from_str(&reserve)
                    .map_err(|_| "--deposit-reserve reserve must be a public key")?;
                let raw = iter
                    .next()
                    .ok_or("--deposit-reserve requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--deposit-reserve amount must be a u64")?;
                if amount == 0 {
                    return Err("--deposit-reserve amount must be greater than 0".to_owned());
                }
                initial_deposit_reserve = Some(reserve);
                initial_deposit_amount_raw = Some(amount);
            }
            "--deposit-idle-vault-reserve" => {
                if idle_vault_deposit_amount_raw.is_some() {
                    return Err("choose only one idle vault deposit mode".to_owned());
                }
                let reserve = iter
                    .next()
                    .ok_or("--deposit-idle-vault-reserve requires a reserve public key")?;
                Pubkey::from_str(&reserve)
                    .map_err(|_| "--deposit-idle-vault-reserve reserve must be a public key")?;
                let raw = iter
                    .next()
                    .ok_or("--deposit-idle-vault-reserve requires an amount in raw USDC units")?;
                let amount = raw
                    .parse::<u64>()
                    .map_err(|_| "--deposit-idle-vault-reserve amount must be a u64")?;
                if amount == 0 {
                    return Err(
                        "--deposit-idle-vault-reserve amount must be greater than 0".to_owned()
                    );
                }
                idle_vault_deposit_reserve = Some(reserve);
                idle_vault_deposit_amount_raw = Some(amount);
            }
            "--execute" => execute = true,
            "--optimization-cycle" => optimization_cycle = true,
            "--reconcile-from-chain" => reconcile_from_chain = true,
            "--reconcile-current-positions" => reconcile_current_positions = true,
            "--reconcile-reserve" => {
                let raw = iter
                    .next()
                    .ok_or("--reconcile-reserve requires a reserve public key")?;
                Pubkey::from_str(&raw).map_err(|_| "--reconcile-reserve must be a public key")?;
                if !reconcile_reserves.iter().any(|reserve| reserve == &raw) {
                    reconcile_reserves.push(raw);
                }
            }
            "--seed-from-user-position" => seed_from_user_position = true,
            "--provision-lookup-table" => provision_lookup_table = true,
            "--provision-route-lookup-table" => provision_route_lookup_table = true,
            "--expected-source-snapshot-id" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-source-snapshot-id requires a value")?;
                expected_source_snapshot_id = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-source-snapshot-id must be an i64")?,
                );
            }
            "--expected-liquidity-mint" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-liquidity-mint requires a mint public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--expected-liquidity-mint must be a public key")?;
                expected_liquidity_mint = Some(raw);
            }
            "--expected-amount-raw" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-amount-raw requires a value")?;
                let amount = raw
                    .parse::<i64>()
                    .map_err(|_| "--expected-amount-raw must be an i64")?;
                if amount <= 0 {
                    return Err("--expected-amount-raw must be greater than 0".to_owned());
                }
                expected_amount_raw = Some(amount);
            }
            "--expected-route-amount-semantics" => {
                expected_route_amount_semantics = Some(
                    iter.next()
                        .ok_or("--expected-route-amount-semantics requires a value")?,
                );
            }
            "--expected-idle-token-account" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-idle-token-account requires a token account public key")?;
                Pubkey::from_str(&raw)
                    .map_err(|_| "--expected-idle-token-account must be a public key")?;
                expected_idle_token_account = Some(raw);
            }
            "--expected-idle-observed-slot" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-idle-observed-slot requires a value")?;
                expected_idle_observed_slot = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-idle-observed-slot must be an i64")?,
                );
            }
            "--expected-idle-observed-at" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-idle-observed-at requires an RFC3339 timestamp")?;
                let parsed = DateTime::parse_from_rfc3339(&raw)
                    .map_err(|_| "--expected-idle-observed-at must be an RFC3339 timestamp")?;
                expected_idle_observed_at = Some(parsed.with_timezone(&Utc));
            }
            "--expected-source-apy-bps" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-source-apy-bps requires a value")?;
                expected_source_apy_bps = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-source-apy-bps must be an i64")?,
                );
            }
            "--expected-target-apy-bps" => {
                let raw = iter
                    .next()
                    .ok_or("--expected-target-apy-bps requires a value")?;
                expected_target_apy_bps = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-target-apy-bps must be an i64")?,
                );
            }
            "--expected-edge-bps" => {
                let raw = iter.next().ok_or("--expected-edge-bps requires a value")?;
                expected_edge_bps = Some(
                    raw.parse::<i64>()
                        .map_err(|_| "--expected-edge-bps must be an i64")?,
                );
            }
            "--rpc-url" => {
                rpc_url = iter.next().ok_or("--rpc-url requires a value")?;
            }
            "--lookup-table" => {
                let raw = iter.next().ok_or("--lookup-table requires a public key")?;
                lookup_tables.push(
                    Pubkey::from_str(&raw).map_err(|_| "--lookup-table must be a public key")?,
                );
            }
            "--help" | "-h" => return Err("help".to_owned()),
            _ => return Err(format!("unknown argument: {arg}")),
        }
    }
    if full_withdraw_main_usdc && full_withdraw_reserve.is_some() {
        return Err(
            "--full-withdraw-main-usdc and --full-withdraw-reserve are aliases; choose one"
                .to_owned(),
        );
    }
    let full_withdraw_requested = full_withdraw_main_usdc || full_withdraw_reserve.is_some();
    if initial_deposit_amount_raw.is_some() && idle_vault_deposit_amount_raw.is_some() {
        return Err(
            "--deposit-idle-vault-reserve cannot be combined with --deposit-main-usdc/--deposit-reserve"
                .to_owned(),
        );
    }
    let selected_special_modes = [
        update_policy,
        initial_deposit_amount_raw.is_some(),
        idle_vault_deposit_amount_raw.is_some(),
        full_withdraw_requested,
        setup_obligation_reserve.is_some(),
        reconcile_current_positions,
        e2e_deposit_amount_raw.is_some(),
        provision_route_lookup_table,
    ]
    .into_iter()
    .filter(|selected| *selected)
    .count();
    if selected_special_modes > 1 {
        return Err(
            "--update-policy, --deposit-main-usdc/--deposit-reserve, --deposit-idle-vault-reserve, --setup-obligation-reserve, --full-withdraw-reserve, --reconcile-current-positions, --e2e-main-prime-main, and --provision-route-lookup-table are mutually exclusive"
                .to_owned(),
        );
    }
    if update_active_policy && !update_policy {
        return Err("--update-active-policy requires --update-policy".to_owned());
    }
    if provision_lookup_table && !update_policy {
        return Err(
            "--provision-lookup-table requires --update-policy; use --provision-route-lookup-table for route ALT provisioning".to_owned(),
        );
    }
    if provision_lookup_table && optimization_cycle {
        return Err(
            "--provision-lookup-table cannot be combined with --optimization-cycle".to_owned(),
        );
    }
    if provision_route_lookup_table {
        if !reconcile_from_chain {
            return Err(
                "--provision-route-lookup-table requires --reconcile-from-chain".to_owned(),
            );
        }
        if source_reserve.is_none() || target_reserve.is_none() {
            return Err(
                "--provision-route-lookup-table requires explicit --source-reserve and --target-reserve"
                    .to_owned(),
            );
        }
        if optimization_cycle {
            return Err(
                "--provision-route-lookup-table is setup-only and cannot be combined with --optimization-cycle"
                    .to_owned(),
            );
        }
        if seed_from_user_position {
            return Err(
                "--provision-route-lookup-table cannot be combined with --seed-from-user-position"
                    .to_owned(),
            );
        }
        if provision_lookup_table {
            return Err(
                "choose either --provision-lookup-table or --provision-route-lookup-table"
                    .to_owned(),
            );
        }
    }
    if source_reserve.is_some() != target_reserve.is_some() {
        return Err("--source-reserve and --target-reserve must be provided together".to_owned());
    }
    if reconcile_current_positions && !reconcile_from_chain {
        return Err("--reconcile-current-positions requires --reconcile-from-chain".to_owned());
    }
    if reconcile_current_positions && reconcile_reserves.is_empty() {
        return Err(
            "--reconcile-current-positions requires at least one --reconcile-reserve".to_owned(),
        );
    }
    if reconcile_current_positions && (execute || seed_from_user_position) {
        return Err("--reconcile-current-positions cannot be combined with --execute or --seed-from-user-position".to_owned());
    }
    if idle_vault_deposit_amount_raw.is_some() {
        if !reconcile_from_chain {
            return Err("--deposit-idle-vault-reserve requires --reconcile-from-chain".to_owned());
        }
        if seed_from_user_position {
            return Err(
                "--deposit-idle-vault-reserve cannot use --seed-from-user-position".to_owned(),
            );
        }
        if execute
            && (expected_idle_token_account.is_none()
                || expected_idle_observed_slot.is_none()
                || expected_idle_observed_at.is_none()
                || expected_liquidity_mint.is_none()
                || expected_amount_raw.is_none()
                || expected_target_apy_bps.is_none()
                || expected_edge_bps.is_none())
        {
            return Err(
                "--deposit-idle-vault-reserve --execute requires --expected-idle-token-account, --expected-idle-observed-slot, --expected-idle-observed-at, --expected-liquidity-mint, --expected-amount-raw, --expected-target-apy-bps, and --expected-edge-bps"
                    .to_owned(),
            );
        }
    }
    if optimization_cycle {
        if !execute {
            return Err("--optimization-cycle requires --execute".to_owned());
        }
        if !reconcile_from_chain {
            return Err("--optimization-cycle requires --reconcile-from-chain".to_owned());
        }
        if source_reserve.is_none() || target_reserve.is_none() {
            return Err(
                "--optimization-cycle requires explicit --source-reserve and --target-reserve"
                    .to_owned(),
            );
        }
        if selected_special_modes != 0 || update_active_policy {
            return Err(
                "--optimization-cycle cannot be combined with setup/admin modes".to_owned(),
            );
        }
        if seed_from_user_position {
            return Err("--optimization-cycle cannot use --seed-from-user-position".to_owned());
        }
        if expected_source_snapshot_id.is_none()
            || expected_liquidity_mint.is_none()
            || expected_amount_raw.is_none()
            || expected_route_amount_semantics.is_none()
            || expected_source_apy_bps.is_none()
            || expected_target_apy_bps.is_none()
            || expected_edge_bps.is_none()
        {
            return Err(
                "--optimization-cycle requires --expected-source-snapshot-id, --expected-liquidity-mint, --expected-amount-raw, --expected-route-amount-semantics, --expected-source-apy-bps, --expected-target-apy-bps, and --expected-edge-bps"
                    .to_owned(),
            );
        }
    }
    Ok(CliOptions {
        settings: settings.ok_or("--settings is required")?,
        vault_index: vault_index.ok_or("--vault-index is required")?,
        direction,
        source_reserve,
        target_reserve,
        update_policy,
        update_active_policy,
        initial_deposit_reserve,
        initial_deposit_amount_raw,
        idle_vault_deposit_reserve,
        idle_vault_deposit_amount_raw,
        full_withdraw_main_usdc,
        full_withdraw_reserve,
        setup_obligation_reserve,
        e2e_deposit_amount_raw,
        execute,
        optimization_cycle,
        reconcile_from_chain,
        reconcile_current_positions,
        reconcile_reserves,
        seed_from_user_position,
        provision_lookup_table,
        provision_route_lookup_table,
        expected_source_snapshot_id,
        expected_liquidity_mint,
        expected_amount_raw,
        expected_route_amount_semantics,
        expected_idle_token_account,
        expected_idle_observed_slot,
        expected_idle_observed_at,
        expected_source_apy_bps,
        expected_target_apy_bps,
        expected_edge_bps,
        rpc_url,
        lookup_tables,
    })
}

fn validate_vault_policy(vault: &SelectedVault) -> Result<(), Box<dyn Error>> {
    if !vault
        .route_modes
        .iter()
        .any(|mode| mode == SAME_MINT_ROUTE_MODE)
    {
        return Err(format!(
            "selected policy {} does not allow {SAME_MINT_ROUTE_MODE}",
            vault.policy_account
        )
        .into());
    }
    Ok(())
}

fn build_same_mint_input(
    options: &CliOptions,
    reserve_move: &ReserveMove,
    vault_id: VaultId,
    positions: &[PositionSummary],
    active_decision: Option<(i64, String)>,
) -> Result<SameMintRebalanceInput, PlanBlocker> {
    if let Some((decision_id, status)) = active_decision {
        return Err(PlanBlocker::ActiveDecision {
            decision_id,
            status,
        });
    }
    if positions.is_empty() {
        return Err(PlanBlocker::MissingCurrentPosition);
    }

    let source_reserve = reserve_move.source_reserve.clone();
    let target_reserve = reserve_move.target_reserve.clone();
    let source = positions
        .iter()
        .find(|position| position.reserve == source_reserve)
        .ok_or_else(|| PlanBlocker::MissingSourceReserve(source_reserve.clone()))?;
    let target = positions
        .iter()
        .find(|position| position.reserve == target_reserve)
        .ok_or_else(|| PlanBlocker::MissingTargetReserve(target_reserve.clone()))?;

    let liquidity_mint = source.liquidity_mint.clone();
    if target.liquidity_mint != liquidity_mint {
        return Err(PlanBlocker::TargetMintMismatch {
            actual: target.liquidity_mint.clone(),
            expected: liquidity_mint,
        });
    }
    if source.amount_raw <= 0 || !source.has_value {
        return Err(PlanBlocker::SourceHasNoValue);
    }
    let evidence =
        route_amount_evidence_from_metadata(source.amount_raw, &source.planning_metadata)
            .ok_or_else(|| PlanBlocker::UnsupportedAmountSemantics {
                reserve: source.reserve.clone(),
                amount_semantics: source
                    .planning_metadata
                    .get("amount_semantics")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned),
            })?;

    let source_apy_bps = options
        .expected_source_apy_bps
        .or(source.supply_apy_bps)
        .unwrap_or_default();
    let target_apy_bps = options
        .expected_target_apy_bps
        .or(target.supply_apy_bps)
        .unwrap_or_default();
    let input = SameMintRebalanceInput {
        vault_id: Some(vault_id),
        settings: None,
        vault_index: None,
        source_reserve,
        target_reserve,
        liquidity_mint,
        amount_raw: evidence.amount_raw,
        route_amount_semantics: evidence.route_amount_semantics,
        source_amount_semantics: evidence.source_amount_semantics,
        source_collateral_amount_raw: evidence.source_collateral_amount_raw,
        redeemable_source_liquidity_amount_raw: evidence.redeemable_source_liquidity_amount_raw,
        idle_vault_liquidity_amount_raw: evidence.idle_vault_liquidity_amount_raw,
        expected_source_snapshot_id: source.snapshot_id,
        source_apy_bps,
        target_apy_bps,
        estimated_edge_bps: options
            .expected_edge_bps
            .unwrap_or(target_apy_bps - source_apy_bps),
        estimated_cost_lamports: 0,
        dry_run: !options.execute,
    };
    validate_monitor_expectations(options, &input)?;
    Ok(input)
}

fn validate_monitor_expectations(
    options: &CliOptions,
    input: &SameMintRebalanceInput,
) -> Result<(), PlanBlocker> {
    if let Some(expected) = options.expected_source_snapshot_id {
        let actual = input.expected_source_snapshot_id.as_i64();
        let accepted_fresh_chain_snapshot =
            options.execute && options.reconcile_from_chain && actual > expected;
        if actual != expected && !accepted_fresh_chain_snapshot {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected source snapshot {expected}, got {}",
                input.expected_source_snapshot_id.as_i64()
            )));
        }
    }
    if let Some(expected) = &options.expected_liquidity_mint {
        if input.liquidity_mint != *expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected liquidity_mint {expected}, got {}",
                input.liquidity_mint
            )));
        }
    }
    if let Some(expected) = options.expected_amount_raw {
        if input.amount_raw != expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected route amount_raw {expected}, got {}",
                input.amount_raw
            )));
        }
    }
    if let Some(expected) = &options.expected_route_amount_semantics {
        if input.route_amount_semantics != *expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected route_amount_semantics {expected}, got {}",
                input.route_amount_semantics
            )));
        }
    }
    if let Some(expected) = options.expected_source_apy_bps {
        if input.source_apy_bps != expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected source_apy_bps {expected}, got {}",
                input.source_apy_bps
            )));
        }
    }
    if let Some(expected) = options.expected_target_apy_bps {
        if input.target_apy_bps != expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected target_apy_bps {expected}, got {}",
                input.target_apy_bps
            )));
        }
    }
    if let Some(expected) = options.expected_edge_bps {
        if input.estimated_edge_bps != expected {
            return Err(PlanBlocker::MonitorPlanDrift(format!(
                "expected estimated_edge_bps {expected}, got {}",
                input.estimated_edge_bps
            )));
        }
    }
    Ok(())
}

fn blocker_report(
    options: &CliOptions,
    reserve_move: &ReserveMove,
    vault: &SelectedVault,
    positions: &[PositionSummary],
    chain_preview: Option<&ChainReconcilePreview>,
    policy_preflight: Option<&PolicyAccountPreflight>,
    user_position_seed: Option<&UserPositionSeedPreview>,
    reconciled_snapshot_id: Option<SnapshotId>,
    blocker: PlanBlocker,
) -> Value {
    json!({
        "status": "blocked_before_decision_write",
        "reason": blocker_reason(&blocker),
        "executeRequested": options.execute,
        "writesDecision": false,
        "wouldReconcileCurrentPositions": options.reconcile_from_chain,
        "reconciledSnapshotId": reconciled_snapshot_id.map(SnapshotId::as_i64),
        "direction": options.direction.as_str(),
        "vault": vault_json(vault),
        "requiredReserves": required_reserves_json(reserve_move),
        "currentPositions": positions.iter().map(position_json).collect::<Vec<_>>(),
        "chainReconcile": chain_preview.map(chain_reconcile_preview_json),
        "userPositionSeed": user_position_seed.map(user_position_seed_preview_json),
        "policyPreflight": policy_route_preflight_json(vault, reserve_move, policy_preflight),
    })
}

fn blocker_reason(blocker: &PlanBlocker) -> Value {
    match blocker {
        PlanBlocker::MissingCurrentPosition => json!("missing_current_positions"),
        PlanBlocker::MissingSourceReserve(reserve) => json!({
            "kind": "missing_source_reserve",
            "reserve": reserve,
        }),
        PlanBlocker::MissingTargetReserve(reserve) => json!({
            "kind": "missing_target_reserve",
            "reserve": reserve,
        }),
        PlanBlocker::SourceHasNoValue => json!("source_reserve_has_no_value"),
        PlanBlocker::TargetMintMismatch { actual, expected } => json!({
            "kind": "target_liquidity_mint_mismatch",
            "actual": actual,
            "expected": expected,
        }),
        PlanBlocker::UnsupportedAmountSemantics {
            reserve,
            amount_semantics,
        } => json!({
            "kind": "unsupported_amount_semantics",
            "reserve": reserve,
            "amountSemantics": amount_semantics,
            "expectedRouteAmountSemantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        }),
        PlanBlocker::MonitorPlanDrift(reason) => json!({
            "kind": "monitor_plan_drift",
            "reason": reason,
        }),
        PlanBlocker::ActiveDecision {
            decision_id,
            status,
        } => json!({
            "kind": "active_decision_exists",
            "decisionId": decision_id,
            "status": status,
        }),
    }
}

fn vault_json(vault: &SelectedVault) -> Value {
    json!({
        "id": vault.id.as_i64(),
        "settings": vault.settings,
        "vaultIndex": vault.vault_index,
        "vaultPubkey": vault.vault_pubkey,
        "policyAccount": vault.policy_account,
        "setupPolicyAccount": vault.setup_policy_account,
        "setupPolicySeed": vault.setup_policy_seed,
        "delegatedSigners": vault.delegated_signers,
        "routeModes": vault.route_modes,
        "kaminoMarkets": vault.kamino_markets,
        "kaminoLiquidityMints": vault.kamino_liquidity_mints,
    })
}

fn required_reserves_json(reserve_move: &ReserveMove) -> Value {
    json!({
        "sourceReserve": reserve_move.source_reserve,
        "targetReserve": reserve_move.target_reserve,
    })
}

fn position_json(position: &PositionSummary) -> Value {
    json!({
        "reserve": position.reserve,
        "liquidityMint": position.liquidity_mint,
        "amountRaw": position.amount_raw.to_string(),
        "hasValue": position.has_value,
        "snapshotId": position.snapshot_id.as_i64(),
        "supplyApyBps": position.supply_apy_bps,
        "planningMetadata": position.planning_metadata,
    })
}

fn same_mint_result_json(result: &SameMintRebalanceResult) -> Value {
    json!({
        "vaultId": result.vault_id.as_i64(),
        "decisionId": result.decision_id.map(|id| id.as_i64()),
        "status": result.status.as_str(),
        "sourceReserve": result.source_reserve,
        "targetReserve": result.target_reserve,
        "liquidityMint": result.liquidity_mint,
        "amountRaw": result.amount_raw.to_string(),
        "signature": result.signature,
        "confirmedSlot": result.confirmed_slot,
        "skipReason": result.skip_reason.map(|reason| reason.decision_reason().as_str()),
        "errorReason": result.error_reason,
        "dryRun": result.dry_run,
        "executionPreview": result.execution_preview.as_ref().map(|preview| json!({
            "kind": preview.kind,
            "sourceReserve": preview.source_reserve,
            "targetReserve": preview.target_reserve,
            "liquidityMint": preview.liquidity_mint,
            "amountRaw": preview.amount_raw.to_string(),
            "routeAmountSemantics": preview.route_amount_semantics,
            "sourceAmountSemantics": preview.source_amount_semantics,
            "sourceCollateralAmountRaw": preview.source_collateral_amount_raw.map(|amount| amount.to_string()),
            "redeemableSourceLiquidityAmountRaw": preview.redeemable_source_liquidity_amount_raw.map(|amount| amount.to_string()),
            "idleVaultLiquidityAmountRaw": preview.idle_vault_liquidity_amount_raw.map(|amount| amount.to_string()),
            "policyExecutions": preview.policy_executions,
            "routeSteps": preview.route_steps,
        })),
    })
}

fn prepared_same_mint_decision_json(decision: &PreparedSameMintDecision) -> Value {
    json!({
        "source": "loyal_yield.rebalance_decisions",
        "id": decision.id.as_i64(),
        "vaultId": decision.vault_id.as_i64(),
        "sourceSnapshotId": decision.source_snapshot_id.as_i64(),
        "sourceReserve": decision.source_reserve,
        "targetReserve": decision.target_reserve,
        "liquidityMint": decision.liquidity_mint,
        "sourceLiquidityMint": decision.source_liquidity_mint,
        "targetLiquidityMint": decision.target_liquidity_mint,
        "amountRaw": decision.amount_raw.to_string(),
        "routeAmountSemantics": decision.execution_plan.get("route_amount_semantics").and_then(Value::as_str),
        "sourceAmountSemantics": decision.execution_plan.get("source_amount_semantics").and_then(Value::as_str),
        "sourceCollateralAmountRaw": plan_i64(&decision.execution_plan, "source_collateral_amount_raw").map(|amount| amount.to_string()),
        "redeemableSourceLiquidityAmountRaw": plan_i64(&decision.execution_plan, "redeemable_source_liquidity_amount_raw").map(|amount| amount.to_string()),
        "idleVaultLiquidityAmountRaw": plan_i64(&decision.execution_plan, "idle_vault_liquidity_amount_raw").map(|amount| amount.to_string()),
        "sourceApyBps": decision.source_apy_bps,
        "targetApyBps": decision.target_apy_bps,
        "estimatedEdgeBps": decision.estimated_edge_bps,
        "estimatedCostLamports": decision.estimated_cost_lamports,
        "executionPlan": decision.execution_plan,
        "idempotencyKey": decision.idempotency_key,
    })
}

fn chain_reconcile_preview_json(preview: &ChainReconcilePreview) -> Value {
    json!({
        "observedSlot": preview.observed_slot,
        "vaultUserMetadata": preview.vault_user_metadata,
        "vaultUserMetadataExists": preview.vault_user_metadata_exists,
        "positions": preview.positions.iter().map(chain_position_json).collect::<Vec<_>>(),
    })
}

fn target_obligation_setup_json(
    preview: &ChainReconcilePreview,
    reserve_move: &ReserveMove,
    vault: &SelectedVault,
    policy_preflight: Option<&PolicyAccountPreflight>,
) -> Option<Value> {
    let target = chain_position_for_reserve(preview, &reserve_move.target_reserve).ok()?;
    let needed = !target.obligation_exists;
    let init_constraint_index = policy_preflight.and_then(|preflight| {
        init_obligation_instruction_constraint_index(Some(preflight), target).ok()
    });
    let decoded_route_policy_allows_init = init_constraint_index.is_some();
    let decoded_route_policy_allows_refresh = policy_preflight
        .map(PolicyAccountPreflight::allows_refresh_obligation)
        .unwrap_or(false);
    let setup_policy_available = vault.setup_policy_account.is_some();
    let (policy_shape, init_policy_source, init_policy_account) =
        if decoded_route_policy_allows_init {
            (
                "route_policy_with_market_scoped_init_obligation",
                Some("route_policy"),
                Some(vault.policy_account.as_str()),
            )
        } else if setup_policy_available {
            (
                "route_policy_plus_setup_policy_market_scoped_init_obligation",
                Some("setup_policy"),
                vault.setup_policy_account.as_deref(),
            )
        } else {
            (
                "route_policy_without_authorized_init_obligation",
                None,
                None,
            )
        };
    let required_before_same_mint_execution = if !needed {
        Vec::<&str>::new()
    } else if decoded_route_policy_allows_init {
        vec![
            "execute route-policy withdraw in the same transaction",
            "execute the target-market init_obligation constraint from the route policy in the same transaction",
            "refresh the newly initialized target obligation before the protected deposit instruction",
        ]
    } else if setup_policy_available {
        vec![
            "execute route-policy withdraw in the same transaction",
            "execute the target-market init_obligation constraint from the setup policy in the same transaction",
            "refresh the newly initialized target obligation before the protected deposit instruction",
        ]
    } else {
        vec!["block execution because no authorized init_obligation policy path is recorded"]
    };

    Some(json!({
        "needed": needed,
        "targetObligation": target.obligation,
        "targetReserve": target.reserve,
        "targetMarket": target.market,
        "vaultUserMetadata": preview.vault_user_metadata,
        "vaultUserMetadataExists": preview.vault_user_metadata_exists,
        "policyShape": policy_shape,
        "initPolicySource": init_policy_source,
        "initPolicyAccount": init_policy_account,
        "setupPolicyAccount": vault.setup_policy_account,
        "setupPolicySeed": vault.setup_policy_seed,
        "decodedRoutePolicyAllowsInitObligation": decoded_route_policy_allows_init,
        "initObligationInstructionConstraintIndex": init_constraint_index,
        "decodedRoutePolicyAllowsRefreshObligation": decoded_route_policy_allows_refresh,
        "requiredBeforeSameMintExecution": required_before_same_mint_execution,
    }))
}

fn missing_obligation_setup_dry_run_json(
    target: &ChainPositionSummary,
    dry_run: &MissingObligationSetupDryRun,
) -> Value {
    json!({
        "targetObligation": target.obligation,
        "targetReserve": target.reserve,
        "targetMarket": target.market,
        "policyAccount": dry_run.policy_account,
        "policySource": dry_run.policy_source,
        "instructionConstraintIndex": dry_run.instruction_constraint_index,
        "initExecution": policy_transaction_json(&dry_run.init_execution),
    })
}

fn missing_obligation_setup_submit_result_json(
    target: &ChainPositionSummary,
    result: &MissingObligationSetupSubmitResult,
) -> Value {
    json!({
        "targetObligation": target.obligation,
        "targetReserve": target.reserve,
        "targetMarket": target.market,
        "policyAccount": result.policy_account,
        "policySource": result.policy_source,
        "instructionConstraintIndex": result.instruction_constraint_index,
        "initExecution": {
            "signature": result.init_signature,
            "submittedSlot": result.init_submitted_slot,
            "confirmedSlot": result.init_confirmed_slot,
            "simulationUnitsConsumed": result.init_simulation_units_consumed,
            "transaction": transaction_packet_json(&result.init_transaction_packet),
        },
    })
}

fn inline_missing_obligation_setup_json(setup: &InlineMissingObligationSetupPreview) -> Value {
    json!({
        "executionMode": "inline_route_transaction",
        "targetObligation": setup.target_obligation,
        "targetReserve": setup.target_reserve,
        "targetMarket": setup.target_market,
        "policyAccount": setup.policy_account,
        "policySource": setup.policy_source,
        "instructionConstraintIndex": setup.instruction_constraint_index,
        "routeOrder": [
            KAMINO_WITHDRAW_ROUTE_STEP,
            KAMINO_INIT_OBLIGATION_ROUTE_STEP,
            KAMINO_DEPOSIT_ROUTE_STEP,
        ],
    })
}

fn route_execution_preview_json(preview: &RouteExecutionPreview) -> Value {
    json!({
        "kind": "squads_program_interaction_same_mint",
        "policyAccount": preview.policy_account,
        "setupPolicyAccount": preview.setup_policy_account,
        "feePayer": preview.fee_payer,
        "signer": preview.signer,
        "accountIndex": preview.account_index,
        "instructionConstraintIndexes": preview.instruction_constraint_indexes,
        "initInstructionConstraintIndex": preview.init_instruction_constraint_index,
        "policyConstraintValidation": preview.policy_constraint_validation.as_ref().map(policy_constraint_validation_json),
        "missingObligationSetup": preview.missing_obligation_setup.as_ref().map(inline_missing_obligation_setup_json),
        "innerInstructionCount": preview.inner_instruction_count,
        "transactionAccountCount": preview.transaction_account_count,
        "outerAccountCount": preview.outer_account_count,
        "setupInstructionProgram": preview.setup_instruction_program,
        "setupInstructionDiscriminator": preview.setup_instruction_discriminator,
        "sourceInstructionProgram": preview.source_instruction_program,
        "targetInstructionProgram": preview.target_instruction_program,
        "sourceInstructionDiscriminator": preview.source_instruction_discriminator,
        "targetInstructionDiscriminator": preview.target_instruction_discriminator,
        "routeSteps": &preview.route_steps,
        "refreshReserves": &preview.refresh_reserves,
    })
}

fn initial_deposit_policy_preview_json(preview: &InitialDepositPolicyPreview) -> Value {
    json!({
        "kind": "squads_program_interaction_initial_main_usdc_deposit",
        "policyAccount": preview.policy_account,
        "signer": preview.signer,
        "accountIndex": preview.account_index,
        "instructionConstraintIndexes": preview.instruction_constraint_indexes,
        "policyConstraintValidation": preview.policy_constraint_validation.as_ref().map(policy_constraint_validation_json),
        "innerInstructionCount": preview.inner_instruction_count,
        "transactionAccountCount": preview.transaction_account_count,
        "outerAccountCount": preview.outer_account_count,
        "setupInstructionProgram": preview.setup_instruction_program,
        "setupInstructionDiscriminator": preview.setup_instruction_discriminator,
        "depositInstructionProgram": preview.deposit_instruction_program,
        "depositInstructionDiscriminator": preview.deposit_instruction_discriminator,
        "routeSteps": &preview.route_steps,
    })
}

fn full_withdraw_policy_preview_json(preview: &FullWithdrawPolicyPreview) -> Value {
    json!({
        "kind": "squads_program_interaction_full_reserve_withdraw",
        "policyAccount": preview.policy_account,
        "signer": preview.signer,
        "accountIndex": preview.account_index,
        "instructionConstraintIndexes": preview.instruction_constraint_indexes,
        "policyConstraintValidation": preview.policy_constraint_validation.as_ref().map(policy_constraint_validation_json),
        "innerInstructionCount": preview.inner_instruction_count,
        "transactionAccountCount": preview.transaction_account_count,
        "outerAccountCount": preview.outer_account_count,
        "withdrawInstructionProgram": preview.withdraw_instruction_program,
        "withdrawInstructionDiscriminator": preview.withdraw_instruction_discriminator,
        "routeSteps": &preview.route_steps,
    })
}

fn policy_constraint_validation_json(validation: &PolicyConstraintValidation) -> Value {
    json!({
        "matches": validation.matches,
        "failures": validation.failures,
    })
}

fn account_proof_json(proof: &AccountProof) -> Value {
    json!({
        "pubkey": proof.pubkey,
        "exists": proof.exists,
        "lamports": proof.lamports.to_string(),
        "owner": proof.owner,
    })
}

fn obligation_account_proof_json(proof: &ObligationAccountProof) -> Value {
    json!({
        "account": account_proof_json(&proof.account),
        "owner": proof.owner,
        "lendingMarket": proof.lending_market,
        "activeDepositCount": proof.active_deposit_count,
        "activeBorrowCount": proof.active_borrow_count,
        "reserveDepositedAmountRaw": proof.reserve_deposited_amount_raw.map(|amount| amount.to_string()),
    })
}

fn chain_position_json(position: &ChainPositionSummary) -> Value {
    json!({
        "reserve": position.reserve,
        "market": position.market,
        "liquidityMint": position.liquidity_mint,
        "liquidityTokenProgram": position.liquidity_token_program,
        "reserveLiquiditySupply": position.reserve_liquidity_supply,
        "collateralMint": position.collateral_mint,
        "reserveCollateralSupply": position.reserve_collateral_supply,
        "collateralFarm": position.collateral_farm,
        "collateralFarmUserState": position.collateral_farm_user_state,
        "collateralFarmUserStateExists": position.collateral_farm_user_state_exists,
        "pythOracle": position.pyth_oracle,
        "switchboardPriceOracle": position.switchboard_price_oracle,
        "switchboardTwapOracle": position.switchboard_twap_oracle,
        "scopePrices": position.scope_prices,
        "obligation": position.obligation,
        "obligationExists": position.obligation_exists,
        "obligationDepositReserves": position.obligation_deposit_reserves,
        "obligationBorrowReserves": position.obligation_borrow_reserves,
        "amountRaw": position.amount_raw.to_string(),
        "hasValue": position.amount_raw > 0,
        "sourceCollateralAmountRaw": position.amount_raw.to_string(),
        "redeemableSourceLiquidityAmountRaw": position.redeemable_liquidity_amount_raw.to_string(),
        "vaultLiquidityAta": position.vault_liquidity_ata,
        "vaultLiquidityTokenAccountExists": position.vault_liquidity_token_account_exists,
        "vaultLiquidityAmountRaw": position.vault_liquidity_amount_raw.to_string(),
        "amountSemantics": AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED,
    })
}

fn user_position_seed_preview_json(preview: &UserPositionSeedPreview) -> Value {
    json!({
        "source": preview.source,
        "rows": preview.rows.iter().map(user_position_seed_row_json).collect::<Vec<_>>(),
        "positions": preview.positions.iter().map(position_json).collect::<Vec<_>>(),
        "amountSemantics": ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY,
        "dryRunOnly": true,
    })
}

fn user_position_seed_row_json(row: &UserPositionSeedRow) -> Value {
    json!({
        "id": row.id,
        "currentReserve": row.current_reserve,
        "currentMarket": row.current_market,
        "currentLiquidityMint": row.current_liquidity_mint,
        "currentAmountRaw": row.current_amount_raw.to_string(),
        "currentObservedSlot": row.current_observed_slot,
        "currentObservedAt": row.current_observed_at,
    })
}

fn policy_account_preflight_json(preflight: &PolicyAccountPreflight) -> Value {
    json!({
        "method": "decoded_squads_policy_account",
        "policyAccount": preflight.policy_account,
        "sourceMarket": preflight.source_market,
        "targetMarket": preflight.target_market,
        "sourceMarketPresent": preflight.decoded.kamino_markets.iter().any(|market| market == &preflight.source_market),
        "targetMarketPresent": preflight.decoded.kamino_markets.iter().any(|market| market == &preflight.target_market),
        "decodedAllowsRequiredMarkets": preflight.allows_required_markets(),
        "decodedAllowsRequiredRouteSteps": preflight.allows_required_route_steps(),
        "decodedAllowsInitObligation": preflight.allows_init_obligation(),
        "decodedAllowsRefreshObligation": preflight.allows_refresh_obligation(),
        "decodedPolicyAccount": decoded_policy_account_json(&preflight.decoded),
    })
}

fn policy_route_preflight_json(
    vault: &SelectedVault,
    reserve_move: &ReserveMove,
    policy_account: Option<&PolicyAccountPreflight>,
) -> Value {
    let source_market = policy_account
        .map(|preflight| preflight.source_market.clone())
        .or_else(|| market_hint_for_reserve(&reserve_move.source_reserve).map(str::to_owned));
    let target_market = policy_account
        .map(|preflight| preflight.target_market.clone())
        .or_else(|| market_hint_for_reserve(&reserve_move.target_reserve).map(str::to_owned));
    let neon_allows_required_markets =
        source_market
            .as_ref()
            .zip(target_market.as_ref())
            .map(|(source_market, target_market)| {
                vault
                    .kamino_markets
                    .iter()
                    .any(|market| market == source_market)
                    && vault
                        .kamino_markets
                        .iter()
                        .any(|market| market == target_market)
            });
    json!({
        "method": "neon_route_policy_with_decoded_policy_account",
        "policyAccount": vault.policy_account,
        "sourceMarket": source_market,
        "targetMarket": target_market,
        "neonAllowsRequiredMarkets": neon_allows_required_markets,
        "neonAllowedLiquidityMints": vault.kamino_liquidity_mints,
        "neonRouteModes": vault.route_modes,
        "policyAccountDecode": policy_account.map(policy_account_preflight_json),
    })
}

fn market_hint_for_reserve(reserve: &str) -> Option<&'static str> {
    if reserve == KAMINO_MAIN_USDC_RESERVE.to_string() {
        Some(KAMINO_MAIN_MARKET)
    } else if reserve == KAMINO_PRIME_USDC_RESERVE {
        Some(KAMINO_PRIME_MARKET)
    } else {
        None
    }
}

fn same_mint_usdc_policy_universe() -> Result<YieldRouteUniverse, Box<dyn Error>> {
    Ok(YieldRouteUniverse::new(
        vec![USDC_MINT],
        vec![
            Pubkey::from_str(KAMINO_MAIN_MARKET)?,
            Pubkey::from_str(KAMINO_PRIME_MARKET)?,
            Pubkey::from_str(KAMINO_MAPLE_MARKET)?,
            Pubkey::from_str(KAMINO_ONRE_MARKET)?,
            Pubkey::from_str(KAMINO_ETHENA_MARKET)?,
        ],
        vec![USDC_MINT],
    ))
}

fn pubkeys_json(pubkeys: &[Pubkey]) -> Vec<String> {
    pubkeys.iter().map(Pubkey::to_string).collect()
}

fn swap_lanes_json(swap_lanes: &[SwapLane]) -> Vec<Value> {
    swap_lanes
        .iter()
        .map(|lane| match lane {
            SwapLane::Jupiter(contract) => json!({
                "kind": "jupiter",
                "programId": contract.program_id.to_string(),
                "exactInDiscriminator": contract.exact_in_discriminator,
                "maxSlippageBps": contract.max_slippage_bps,
            }),
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => json!({
                "kind": "loyal_hub",
                "hubAuthorizer": hub_authorizer.to_string(),
                "maxFeeBps": max_fee_bps,
            }),
        })
        .collect()
}

fn policy_swap_lanes_json(
    setup: &YieldRouteActionSetup,
    swap_lanes: &[SwapLane],
) -> Result<Value, Box<dyn Error>> {
    let action_account = setup.accounts.withdraw.to_string();
    let deposit_index = u8::try_from(1 + swap_lanes.len())?;
    let lanes = swap_lanes
        .iter()
        .enumerate()
        .map(|(offset, lane)| -> Result<Value, Box<dyn Error>> {
            let swap_index = u8::try_from(1 + offset)?;
            Ok(match lane {
                SwapLane::Jupiter(contract) => json!({
                    "lane": "jupiter",
                    "programId": contract.program_id.to_string(),
                    "exactInDiscriminator": contract.exact_in_discriminator,
                    "maxSlippageBps": contract.max_slippage_bps,
                    "actionAccount": action_account.clone(),
                    "instructionConstraintIndexes": [0_u8, swap_index, deposit_index],
                }),
                SwapLane::LoyalHub {
                    hub_authorizer,
                    max_fee_bps,
                } => json!({
                    "lane": "loyal_hub",
                    "hubAuthorizer": hub_authorizer.to_string(),
                    "maxFeeBps": max_fee_bps,
                    "actionAccount": action_account.clone(),
                    "instructionConstraintIndexes": [0_u8, swap_index, deposit_index],
                }),
            })
        })
        .collect::<Result<Vec<_>, Box<dyn Error>>>()?;
    Ok(Value::Array(lanes))
}

fn decoded_policy_account_json(decoded: &DecodedPolicyAccount) -> Value {
    json!({
        "layout": decoded.layout.as_str(),
        "delegatedSigners": decoded.delegated_signers,
        "threshold": decoded.threshold,
        "accountIndex": decoded.account_index,
        "instructionCount": decoded.instruction_count,
        "kaminoMarkets": decoded.kamino_markets,
        "kaminoLiquidityMints": decoded.kamino_liquidity_mints,
        "instructions": decoded.instructions.iter().map(decoded_policy_instruction_json).collect::<Vec<_>>(),
    })
}

fn decoded_policy_instruction_json(instruction: &DecodedPolicyInstructionSummary) -> Value {
    json!({
        "programId": instruction.program_id,
        "routeStep": instruction.route_step,
        "dataDiscriminator": instruction.data_discriminator,
        "markets": instruction.markets,
        "liquidityMints": instruction.liquidity_mints,
        "accountConstraints": instruction.account_constraints.iter().map(decoded_policy_account_constraint_json).collect::<Vec<_>>(),
    })
}

fn decoded_policy_account_constraint_json(
    constraint: &DecodedPolicyAccountConstraintSummary,
) -> Value {
    json!({
        "accountIndex": constraint.account_index,
        "kind": constraint.kind,
        "pubkeys": constraint.pubkeys,
        "owner": constraint.owner,
        "dataConstraints": constraint.data_constraints.iter().map(decoded_policy_data_constraint_json).collect::<Vec<_>>(),
    })
}

fn decoded_policy_data_constraint_json(constraint: &DecodedPolicyDataConstraintSummary) -> Value {
    json!({
        "dataOffset": constraint.data_offset,
        "operator": constraint.operator,
        "value": constraint.value,
    })
}

fn same_mint_input_json(input: &SameMintRebalanceInput) -> Value {
    json!({
        "vaultId": input.vault_id.map(VaultId::as_i64),
        "sourceReserve": input.source_reserve,
        "targetReserve": input.target_reserve,
        "liquidityMint": input.liquidity_mint,
        "amountRaw": input.amount_raw.to_string(),
        "routeAmountSemantics": input.route_amount_semantics,
        "sourceAmountSemantics": input.source_amount_semantics,
        "sourceCollateralAmountRaw": input.source_collateral_amount_raw.map(|amount| amount.to_string()),
        "redeemableSourceLiquidityAmountRaw": input.redeemable_source_liquidity_amount_raw.map(|amount| amount.to_string()),
        "idleVaultLiquidityAmountRaw": input.idle_vault_liquidity_amount_raw.map(|amount| amount.to_string()),
        "sourceSnapshotId": input.expected_source_snapshot_id.as_i64(),
        "sourceApyBps": input.source_apy_bps,
        "targetApyBps": input.target_apy_bps,
        "estimatedEdgeBps": input.estimated_edge_bps,
        "estimatedCostLamports": input.estimated_cost_lamports,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_chain_position(
        collateral_amount: u64,
        redeemable_liquidity_amount: u64,
    ) -> ChainPositionSummary {
        ChainPositionSummary {
            reserve: "source-reserve".to_owned(),
            market: "market".to_owned(),
            liquidity_mint: "mint".to_owned(),
            liquidity_token_program: "token-program".to_owned(),
            reserve_liquidity_supply: "liquidity-supply".to_owned(),
            collateral_mint: "collateral-mint".to_owned(),
            reserve_collateral_supply: "collateral-supply".to_owned(),
            collateral_farm: None,
            collateral_farm_user_state: None,
            collateral_farm_user_state_exists: false,
            pyth_oracle: None,
            switchboard_price_oracle: None,
            switchboard_twap_oracle: None,
            scope_prices: None,
            obligation: "obligation".to_owned(),
            obligation_exists: true,
            obligation_deposit_reserves: Vec::new(),
            obligation_borrow_reserves: Vec::new(),
            amount_raw: collateral_amount,
            redeemable_liquidity_amount_raw: redeemable_liquidity_amount,
            vault_liquidity_ata: "vault-liquidity-ata".to_owned(),
            vault_liquidity_token_account_exists: true,
            vault_liquidity_amount_raw: 0,
        }
    }

    fn test_same_mint_input(
        route_liquidity_amount: i64,
        source_collateral_amount: Option<i64>,
    ) -> SameMintRebalanceInput {
        SameMintRebalanceInput {
            vault_id: None,
            settings: None,
            vault_index: None,
            source_reserve: "source-reserve".to_owned(),
            target_reserve: "target-reserve".to_owned(),
            liquidity_mint: "mint".to_owned(),
            amount_raw: route_liquidity_amount,
            route_amount_semantics: ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            source_amount_semantics: Some(AMOUNT_SEMANTICS_KAMINO_COLLATERAL_DEPOSITED.to_owned()),
            source_collateral_amount_raw: source_collateral_amount,
            redeemable_source_liquidity_amount_raw: Some(route_liquidity_amount),
            idle_vault_liquidity_amount_raw: Some(0),
            expected_source_snapshot_id: SnapshotId(1),
            source_apy_bps: 100,
            target_apy_bps: 200,
            estimated_edge_bps: 100,
            estimated_cost_lamports: 5_000,
            dry_run: false,
        }
    }

    fn test_cli_options(
        execute: bool,
        reconcile_from_chain: bool,
        expected_source_snapshot_id: Option<i64>,
    ) -> CliOptions {
        CliOptions {
            settings: "settings".to_owned(),
            vault_index: 1,
            direction: Direction::MainToPrime,
            source_reserve: Some("source-reserve".to_owned()),
            target_reserve: Some("target-reserve".to_owned()),
            update_policy: false,
            update_active_policy: false,
            initial_deposit_reserve: None,
            initial_deposit_amount_raw: None,
            idle_vault_deposit_reserve: None,
            idle_vault_deposit_amount_raw: None,
            full_withdraw_main_usdc: false,
            full_withdraw_reserve: None,
            setup_obligation_reserve: None,
            e2e_deposit_amount_raw: None,
            execute,
            optimization_cycle: true,
            reconcile_from_chain,
            reconcile_current_positions: false,
            reconcile_reserves: Vec::new(),
            seed_from_user_position: false,
            provision_lookup_table: false,
            provision_route_lookup_table: false,
            expected_source_snapshot_id,
            expected_liquidity_mint: Some("mint".to_owned()),
            expected_amount_raw: Some(480_000_000),
            expected_route_amount_semantics: Some(
                ROUTE_AMOUNT_SEMANTICS_REDEEMABLE_LIQUIDITY.to_owned(),
            ),
            expected_idle_token_account: None,
            expected_idle_observed_slot: None,
            expected_idle_observed_at: None,
            expected_source_apy_bps: Some(100),
            expected_target_apy_bps: Some(200),
            expected_edge_bps: Some(100),
            rpc_url: "http://localhost:8899".to_owned(),
            lookup_tables: Vec::new(),
        }
    }

    #[test]
    fn rejects_lookup_table_provisioning_during_optimization_cycle() {
        let error = parse_args(vec![
            "--settings".to_owned(),
            Pubkey::new_unique().to_string(),
            "--vault-index".to_owned(),
            "1".to_owned(),
            "--update-policy".to_owned(),
            "--provision-lookup-table".to_owned(),
            "--optimization-cycle".to_owned(),
        ])
        .unwrap_err();

        assert_eq!(
            error,
            "--provision-lookup-table cannot be combined with --optimization-cycle"
        );
    }

    #[test]
    fn accepts_explicit_route_lookup_table_provisioning_mode() {
        let source_reserve = Pubkey::new_unique();
        let target_reserve = Pubkey::new_unique();

        let options = parse_args(vec![
            "--settings".to_owned(),
            Pubkey::new_unique().to_string(),
            "--vault-index".to_owned(),
            "1".to_owned(),
            "--provision-route-lookup-table".to_owned(),
            "--reconcile-from-chain".to_owned(),
            "--source-reserve".to_owned(),
            source_reserve.to_string(),
            "--target-reserve".to_owned(),
            target_reserve.to_string(),
        ])
        .expect("explicit route ALT provisioning mode should parse");

        assert!(options.provision_route_lookup_table);
        assert!(!options.optimization_cycle);
        assert_eq!(options.source_reserve, Some(source_reserve.to_string()));
        assert_eq!(options.target_reserve, Some(target_reserve.to_string()));
    }

    #[test]
    fn rejects_route_lookup_table_provisioning_with_user_position_seed() {
        let source_reserve = Pubkey::new_unique();
        let target_reserve = Pubkey::new_unique();

        let error = parse_args(vec![
            "--settings".to_owned(),
            Pubkey::new_unique().to_string(),
            "--vault-index".to_owned(),
            "1".to_owned(),
            "--provision-route-lookup-table".to_owned(),
            "--reconcile-from-chain".to_owned(),
            "--seed-from-user-position".to_owned(),
            "--source-reserve".to_owned(),
            source_reserve.to_string(),
            "--target-reserve".to_owned(),
            target_reserve.to_string(),
        ])
        .unwrap_err();

        assert_eq!(
            error,
            "--provision-route-lookup-table cannot be combined with --seed-from-user-position"
        );
    }

    #[test]
    fn route_lookup_table_provisioning_execute_does_not_write_current_positions() {
        let mut options = test_cli_options(true, true, Some(1));
        options.optimization_cycle = false;
        options.provision_route_lookup_table = true;
        options.seed_from_user_position = true;

        assert!(!writes_current_positions_from_chain(&options));
        assert!(!writes_current_positions_from_user_seed(&options));
        assert!(uses_chain_preview_positions(&options, true));
    }

    #[test]
    fn live_route_execution_still_writes_current_positions_from_chain() {
        let options = test_cli_options(true, true, Some(1));
        let mut seed_options = test_cli_options(true, false, Some(1));
        seed_options.seed_from_user_position = true;

        assert!(writes_current_positions_from_chain(&options));
        assert!(writes_current_positions_from_user_seed(&seed_options));
        assert!(!uses_chain_preview_positions(&options, true));
    }

    #[test]
    fn rejects_lookup_table_mutations_outside_provisioning_mode() {
        let authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let (create_instruction, lookup_table_address) =
            address_lookup_table_instruction::create_lookup_table(authority, payer, 42);
        let extend_instruction = address_lookup_table_instruction::extend_lookup_table(
            lookup_table_address,
            authority,
            Some(payer),
            vec![Pubkey::new_unique()],
        );

        let create_error = guard_lookup_table_mutations(
            &[create_instruction.clone()],
            AltInstructionMode::RejectProvisioning,
            "route execution",
        )
        .unwrap_err()
        .to_string();
        let extend_error = guard_lookup_table_mutations(
            &[extend_instruction.clone()],
            AltInstructionMode::RejectProvisioning,
            "route execution",
        )
        .unwrap_err()
        .to_string();

        assert!(create_error.contains("Address Lookup Table create instruction"));
        assert!(extend_error.contains("Address Lookup Table extend instruction"));
        guard_lookup_table_mutations(
            &[create_instruction, extend_instruction],
            AltInstructionMode::AllowProvisioning,
            "route provisioning",
        )
        .expect("explicit provisioning mode should allow ALT mutations");
    }

    #[test]
    fn route_lookup_table_address_hash_is_order_independent() {
        let first = Pubkey::new_unique();
        let second = Pubkey::new_unique();
        let third = Pubkey::new_unique();

        let ordered_hash = route_lookup_table_address_hash(&[first, second, third]);
        let shuffled_hash = route_lookup_table_address_hash(&[third, first, second]);

        assert_eq!(ordered_hash, shuffled_hash);
    }

    #[test]
    fn lookup_table_missing_coverage_fails_closed() {
        let missing = vec![Pubkey::new_unique(), Pubkey::new_unique()];

        let error = ensure_route_lookup_table_coverage("same_mint_kamino:test", &missing)
            .unwrap_err()
            .to_string();

        assert!(error.starts_with(
            "lookup_table_coverage_missing: route scope same_mint_kamino:test is missing 2 required address(es): "
        ));
        assert!(error.contains(&missing[0].to_string()));
        assert!(error.contains(&missing[1].to_string()));
        ensure_route_lookup_table_coverage("same_mint_kamino:test", &[])
            .expect("complete lookup-table coverage should pass");
    }

    #[test]
    fn route_lookup_table_reuse_json_reports_missing_coverage() {
        let missing = Pubkey::new_unique();
        let fee_payer = Pubkey::new_unique();
        let fee_payer_string = fee_payer.to_string();
        let mut options = test_cli_options(true, true, Some(1));
        options.rpc_url = "https://api.mainnet-beta.solana.com".to_owned();
        let coverage = RouteLookupTableCoverage {
            scope: "same_mint_kamino:test".to_owned(),
            lookup_table_accounts: Vec::new(),
            required_addresses: vec![missing],
            missing_addresses: vec![missing],
        };

        let json = coverage.reuse_only_json(&options, fee_payer);

        assert_eq!(
            json.get("status").and_then(Value::as_str),
            Some("lookup_table_coverage_missing")
        );
        assert_eq!(json.get("execute").and_then(Value::as_bool), Some(true));
        assert_eq!(
            json.get("missingBeforeProvisionCount")
                .and_then(Value::as_u64),
            Some(1)
        );
        assert_eq!(
            json.get("scope").and_then(Value::as_str),
            Some("same_mint_kamino:test")
        );
        assert_eq!(
            json.get("authority").and_then(Value::as_str),
            Some(fee_payer_string.as_str())
        );
    }

    #[test]
    fn lookup_table_warmup_slot_uses_ready_slot() {
        assert_eq!(
            lookup_table_warmup_slot(&json!({
                "lastExtendedSlot": 10,
                "readySlot": 11,
                "ready": true
            })),
            Some(11)
        );
        assert_eq!(
            lookup_table_warmup_slot(&json!({
                "usableSlot": 12
            })),
            Some(12)
        );
    }

    #[test]
    fn lookup_table_missing_addresses_include_only_uncovered_required_keys() {
        let covered_first = Pubkey::new_unique();
        let covered_second = Pubkey::new_unique();
        let missing = Pubkey::new_unique();
        let lookup_table_accounts = vec![AddressLookupTableAccount {
            key: Pubkey::new_unique(),
            addresses: vec![covered_second, covered_first, Pubkey::new_unique()],
        }];

        let missing_addresses = missing_lookup_table_addresses(
            &[covered_first, missing, covered_second],
            &lookup_table_accounts,
        );

        assert_eq!(missing_addresses, vec![missing]);
    }

    #[test]
    fn collateral_conversion_can_produce_distinct_redeemable_liquidity() {
        let scale = BigUint::from(1_u128 << 60);
        let total_liquidity_scaled = BigUint::from(1_200_000_000_u64) * scale;

        let redeemable = collateral_to_redeemable_liquidity_amount(
            1_000_000_000,
            &total_liquidity_scaled,
            500_000_000,
        )
        .expect("conversion should fit");

        assert_eq!(redeemable, 600_000_000);
    }

    #[test]
    fn source_collateral_validation_rejects_route_liquidity_as_withdraw_amount() {
        let source = test_chain_position(404_323_479, 480_000_000);
        let input = test_same_mint_input(480_000_000, Some(480_000_000));

        let error = planned_source_collateral_amount(&input, &source).unwrap_err();

        assert!(
            error
                .to_string()
                .contains("does not match planned source_collateral_amount_raw"),
            "unexpected error: {error}"
        );
    }

    #[test]
    fn source_collateral_validation_accepts_distinct_collateral_and_liquidity() {
        let source = test_chain_position(404_323_479, 480_000_000);
        let input = test_same_mint_input(480_000_000, Some(404_323_479));

        let amount = planned_source_collateral_amount(&input, &source)
            .expect("matching source collateral should pass");

        assert_eq!(amount, 404_323_479);
    }

    #[test]
    fn monitor_expectations_accept_newer_execute_chain_snapshot() {
        let options = test_cli_options(true, true, Some(1));
        let mut input = test_same_mint_input(480_000_000, Some(404_323_479));
        input.expected_source_snapshot_id = SnapshotId(2);

        validate_monitor_expectations(&options, &input)
            .expect("execute plus chain reconcile should accept its fresh snapshot");
    }

    #[test]
    fn monitor_expectations_reject_snapshot_drift_without_chain_reconcile() {
        let options = test_cli_options(true, false, Some(1));
        let mut input = test_same_mint_input(480_000_000, Some(404_323_479));
        input.expected_source_snapshot_id = SnapshotId(2);

        let error = validate_monitor_expectations(&options, &input).unwrap_err();

        assert_eq!(
            blocker_reason(&error),
            json!({
                "kind": "monitor_plan_drift",
                "reason": "expected source snapshot 1, got 2",
            })
        );
    }
}

fn print_help() {
    println!(
         "Usage: same-mint-reserve-swap --settings <PUBKEY> --vault-index <N> [--e2e-main-prime-main <AMOUNT_RAW>] [--update-policy] [--update-active-policy] [--provision-lookup-table] [--provision-route-lookup-table] [--deposit-main-usdc <AMOUNT_RAW> | --deposit-reserve <RESERVE> <AMOUNT_RAW> | --deposit-idle-vault-reserve <RESERVE> <AMOUNT_RAW>] [--setup-obligation-reserve <RESERVE>] [--full-withdraw-main-usdc | --full-withdraw-reserve <RESERVE>] [--direction main-to-prime|prime-to-main | --source-reserve <PUBKEY> --target-reserve <PUBKEY>] [--optimization-cycle] [--reconcile-from-chain] [--seed-from-user-position] [--rpc-url <URL>] [--lookup-table <PUBKEY>...] [--execute]\n\n\
         Dry-run is the default. Reads NEON_DATABASE_URL, optionally SOLANA_RPC_URL, and optionally YIELD_ROUTE_LOOKUP_TABLES from the environment. E2E mode runs policy update, initial Main USDC deposit, Main -> Prime move, Prime -> Main move, and full Main withdrawal as child invocations of this same binary. Policy update mode uses SOLANA_TESTING_PK for the settings authority and POLICY_KEYPAIR as the delegated policy signer. By default --update-policy targets a fresh next policy seed; add --update-active-policy to intentionally update the currently active DB policy instead. Add --setup-obligation-reserve <reserve> as a setup/admin-only mode to execute the decoded target-market init_obligation constraint from the route or setup policy. Add --optimization-cycle for live same-mint route execution; that mode requires explicit source/target reserves plus --reconcile-from-chain --execute, uses POLICY_KEYPAIR as fee payer and delegated signer, reuses durable lookup-table coverage, and fails before route send if coverage is missing. Add --provision-route-lookup-table with explicit source/target reserves plus --reconcile-from-chain for route lookup-table setup; it uses POLICY_KEYPAIR as the lookup-table authority and payer, cannot be combined with --optimization-cycle or --seed-from-user-position, and exits without writing a rebalance decision or sending the route. Add --deposit-idle-vault-reserve for router-owned USDC already inside the vault; execute mode requires expected idle token account, observed slot/time, mint, amount, target APY, and edge, uses POLICY_KEYPAIR as fee payer/delegated signer, and does not read SOLANA_TESTING_PK. Add --provision-lookup-table only with --update-policy for durable policy lookup-table setup. Initial deposit mode uses SOLANA_TESTING_PK as the funding wallet and POLICY_KEYPAIR for the policy deposit; --deposit-reserve allows choosing a non-Main Safe USDC reserve when Main is already the APY winner. Full withdraw mode uses POLICY_KEYPAIR for the policy withdraw, then SOLANA_TESTING_PK authority cleanup to recover vault USDC, close the route policy plus setup policy when present, and report rent cleanup proof. Run through:\n\
         op run --env-file=.env.1password -- bun run same-mint:swap -- --settings <PUBKEY> --vault-index 1 --reconcile-from-chain --seed-from-user-position"
    );
}
