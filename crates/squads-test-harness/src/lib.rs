use borsh::BorshSerialize;
use litesvm::LiteSVM;
use solana_sdk::{
    account::Account,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    message::Message,
    pubkey,
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use spl_token::solana_program::{program_option::COption, program_pack::Pack};
use std::{env, fs, io::Write, path::PathBuf};

pub const SQUADS_SMART_ACCOUNT_PROGRAM_ID: Pubkey =
    pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
pub const SQUADS_SEED_SETTINGS: &[u8] = b"settings";
pub const SQUADS_SEED_SMART_ACCOUNT: &[u8] = b"smart_account";
pub const SQUADS_SEED_POLICY: &[u8] = b"policy";
pub const SQUADS_PROGRAM_CONFIG_SEED: &[u8] = b"program_config";
pub const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
pub const SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR: [u8; 8] = [197, 102, 253, 231, 77, 84, 50, 17];
pub const SQUADS_PROGRAM_CONFIG_DISCRIMINATOR: [u8; 8] = [196, 210, 90, 231, 144, 149, 140, 63];
pub const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
pub const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
pub const SQUADS_ONE_SIGNER_SETTINGS_SPACE: usize = 168;
pub const DEFAULT_WALLET_AIRDROP_LAMPORTS: u64 = 1_000_000_000;
pub const LAMPORTS_PER_SOL: u64 = 1_000_000_000;
pub const SOL_DECIMALS: u8 = 9;
pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const WRAPPED_SOL_MINT: Pubkey = pubkey!("So11111111111111111111111111111111111111112");
pub const USDC_MINT: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const PYUSD_MINT: Pubkey = pubkey!("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
pub const USDC_DECIMALS: u8 = 6;
pub const PYUSD_DECIMALS: u8 = 6;
pub const MOCK_JUPITER_SOL_TO_USDC: u8 = 1;
pub const MOCK_JUPITER_USDC_TO_PYUSD: u8 = 2;
pub const MOCK_JUPITER_STABLE_EXACT_IN: u8 = 3;
pub const LOYAL_HUB_SWAP_PROGRAM_ID: Pubkey = Pubkey::new_from_array([42; 32]);
pub const LOYAL_HUB_INITIALIZE_CONFIG: u8 = 0;
pub const LOYAL_HUB_SWAP_EXACT_IN: u8 = 1;
pub const LOYAL_HUB_WITHDRAW_INVENTORY: u8 = 2;
pub const LOYAL_HUB_SET_PAUSED: u8 = 3;
pub const LOYAL_HUB_SET_CONFIG: u8 = 4;
pub const LOYAL_HUB_CONFIG_SEED: &[u8] = b"config";
pub const LOYAL_HUB_AUTHORITY_SEED: &[u8] = b"hub-authority";
pub const JUPITER_SWAP_AUTHORITY_SEED: &[u8] = b"jupiter-swap-authority";
pub const MOCK_JUPITER_USDC_RESERVE_TOKEN_ACCOUNT_SEED: &[u8] =
    b"mock-jupiter-usdc-reserve-token-account";
pub const MOCK_JUPITER_PYUSD_RESERVE_TOKEN_ACCOUNT_SEED: &[u8] =
    b"mock-jupiter-pyusd-reserve-token-account";
pub const MOCK_JUPITER_STABLE_RESERVE_TOKEN_ACCOUNT_SEED: &[u8] = b"mock-jupiter-stable-reserve";
pub const LOYAL_HUB_TOKEN_ACCOUNT_SEED: &[u8] = b"loyal-hub-token-account";
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KvauGMspG5k6rtzrqqn7WNn3oZdyKqLKwK2XWQ8FLjd");
pub const KAMINO_MAIN_MARKET: Pubkey = pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF");
pub const KAMINO_MAIN_USDC_RESERVE: Pubkey =
    pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59");
pub const KAMINO_MAIN_PYUSD_RESERVE: Pubkey =
    pubkey!("2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN");
pub const KAMINO_PRIME_MARKET: Pubkey = pubkey!("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA");
pub const KAMINO_PRIME_USDC_RESERVE: Pubkey =
    pubkey!("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu");
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [242, 35, 198, 137, 82, 225, 242, 182];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];
pub const KAMINO_COLLATERAL_DECIMALS: u8 = 6;
pub const KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED: &[u8] = b"kamino-reserve-liq-authority";
pub const KAMINO_COLLATERAL_MINT_AUTHORITY_SEED: &[u8] = b"kamino-collateral-mint-authority";
pub const MOCK_YIELD_PROTOCOLS_PROGRAM_SO_ENV: &str = "MOCK_YIELD_PROTOCOLS_PROGRAM_SO";
pub const MOCK_YIELD_PROTOCOLS_PROGRAM_SO: &str = "mock_yield_protocols_program.so";
pub const LOYAL_HUB_SWAP_PROGRAM_SO_ENV: &str = "LOYAL_HUB_SWAP_PROGRAM_SO";
pub const LOYAL_HUB_SWAP_PROGRAM_SO: &str = "loyal_hub_swap_program.so";
pub const SQUADS_SMART_ACCOUNT_PROGRAM_SO_FIXTURE: &str =
    "crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so";
pub const YIELD_ROUTE_WITHDRAW_POLICY_SEED: u64 = 1;
pub const YIELD_ROUTE_SWAP_POLICY_SEED: u64 = 2;
pub const YIELD_ROUTE_DEPOSIT_POLICY_SEED: u64 = 3;
pub const YIELD_ROUTE_STANDALONE_POLICY_SEED: u64 = 1;

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSettingsAction {
    AddSigner {
        new_signer: SquadsSmartAccountSigner,
    },
    RemoveSigner {
        old_signer: Pubkey,
    },
    ChangeThreshold {
        new_threshold: u16,
    },
    SetTimeLock {
        new_time_lock: u32,
    },
    AddSpendingLimit {
        seed: Pubkey,
        account_index: u8,
        mint: Pubkey,
        amount: u64,
        period: LegacyPeriod,
        signers: Vec<Pubkey>,
        destinations: Vec<Pubkey>,
        expiration: i64,
    },
    RemoveSpendingLimit {
        spending_limit: Pubkey,
    },
    SetArchivalAuthority {
        new_archival_authority: Option<Pubkey>,
    },
    PolicyCreate {
        seed: u64,
        policy_creation_payload: SquadsPolicyCreationPayload,
        signers: Vec<SquadsSmartAccountSigner>,
        threshold: u16,
        time_lock: u32,
        start_timestamp: Option<i64>,
        expiration_args: Option<SquadsPolicyExpirationArgs>,
    },
    PolicyUpdate {
        policy: Pubkey,
        signers: Vec<SquadsSmartAccountSigner>,
        threshold: u16,
        time_lock: u32,
        policy_update_payload: SquadsPolicyCreationPayload,
        expiration_args: Option<SquadsPolicyExpirationArgs>,
    },
    PolicyRemove {
        policy: Pubkey,
    },
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum LegacyPeriod {
    OneTime,
    Day,
    Week,
    Month,
}

#[derive(BorshSerialize)]
struct SquadsSmartAccountSigner {
    key: Pubkey,
    permissions: SquadsPermissions,
}

#[derive(BorshSerialize)]
struct SquadsPermissions {
    mask: u8,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyExpirationArgs {
    Timestamp(i64),
    SettingsState,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyCreationPayload {
    InternalFundTransfer(Vec<u8>),
    SpendingLimit(SquadsSpendingLimitPolicyCreationPayload),
    SettingsChange(Vec<u8>),
    LegacyProgramInteraction(SquadsProgramInteractionPolicyCreationPayloadLegacy),
    ProgramInteraction(SquadsProgramInteractionPolicyCreationPayload),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPolicyCreationPayloadLegacy {
    account_index: u8,
    instructions_constraints: Vec<SquadsInstructionConstraint>,
    pre_hook: Option<SquadsHook>,
    post_hook: Option<SquadsHook>,
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPolicyCreationPayload {
    account_index: u8,
    pubkey_table: SquadsSmallVec<Pubkey>,
    instructions_constraints: SquadsSmallVec<SquadsCompiledInstructionConstraint>,
    pre_hook: Option<SquadsCompiledHook>,
    post_hook: Option<SquadsCompiledHook>,
    spending_limits: SquadsSmallVec<SquadsCompiledLimitedSpendingLimit>,
}

#[derive(Clone)]
struct SquadsSmallVec<T>(Vec<T>);

impl<T> From<Vec<T>> for SquadsSmallVec<T> {
    fn from(value: Vec<T>) -> Self {
        Self(value)
    }
}

impl<T: BorshSerialize> BorshSerialize for SquadsSmallVec<T> {
    fn serialize<W: Write>(&self, writer: &mut W) -> std::io::Result<()> {
        let len = u8::try_from(self.0.len()).map_err(|_| std::io::ErrorKind::InvalidInput)?;
        writer.write_all(&[len])?;
        for item in &self.0 {
            item.serialize(writer)?;
        }
        Ok(())
    }
}

#[derive(BorshSerialize)]
struct SquadsCompiledInstructionConstraint {
    program_id_index: u8,
    account_constraints: SquadsSmallVec<SquadsCompiledAccountConstraint>,
    data_constraints: SquadsSmallVec<SquadsDataConstraint>,
}

#[derive(BorshSerialize)]
struct SquadsCompiledAccountConstraint {
    account_index: u8,
    account_constraint: SquadsCompiledAccountConstraintType,
    owner_index: Option<u8>,
}

#[derive(BorshSerialize)]
enum SquadsCompiledAccountConstraintType {
    Pubkey(SquadsSmallVec<u8>),
    AccountData(SquadsSmallVec<SquadsDataConstraint>),
}

#[derive(BorshSerialize)]
struct SquadsCompiledHook {
    num_extra_accounts: u8,
    account_constraints: SquadsSmallVec<SquadsCompiledAccountConstraint>,
    instruction_data: SquadsSmallVec<u8>,
    program_id_index: u8,
    pass_inner_instructions: bool,
}

#[derive(BorshSerialize)]
struct SquadsCompiledLimitedSpendingLimit {
    mint_index: u8,
    time_constraints: SquadsLimitedTimeConstraints,
    quantity_constraints: SquadsLimitedQuantityConstraints,
}

#[derive(BorshSerialize, Clone)]
struct SquadsInstructionConstraint {
    program_id: Pubkey,
    account_constraints: Vec<SquadsAccountConstraint>,
    data_constraints: Vec<SquadsDataConstraint>,
}

#[derive(BorshSerialize)]
struct SquadsHook {
    num_extra_accounts: u8,
    account_constraints: Vec<SquadsAccountConstraint>,
    instruction_data: Vec<u8>,
    program_id: Pubkey,
    pass_inner_instructions: bool,
}

#[derive(BorshSerialize, Clone)]
struct SquadsAccountConstraint {
    account_index: u8,
    account_constraint: SquadsAccountConstraintType,
    owner: Option<Pubkey>,
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
enum SquadsAccountConstraintType {
    Pubkey(Vec<Pubkey>),
    AccountData(Vec<SquadsDataConstraint>),
}

#[derive(BorshSerialize, Clone)]
struct SquadsDataConstraint {
    data_offset: u64,
    data_value: SquadsDataValue,
    operator: SquadsDataOperator,
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
enum SquadsDataValue {
    U8(u8),
    U16Le(u16),
    U32Le(u32),
    U64Le(u64),
    U128Le(u128),
    U8Slice(Vec<u8>),
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
enum SquadsDataOperator {
    Equals,
    NotEquals,
    GreaterThan,
    GreaterThanOrEqualTo,
    LessThan,
    LessThanOrEqualTo,
}

#[derive(BorshSerialize)]
struct SquadsLimitedSpendingLimit {
    mint: Pubkey,
    time_constraints: SquadsLimitedTimeConstraints,
    quantity_constraints: SquadsLimitedQuantityConstraints,
}

#[derive(BorshSerialize, Clone)]
struct SquadsLimitedTimeConstraints {
    start: i64,
    expiration: Option<i64>,
    period: SquadsPeriodV2,
}

#[derive(BorshSerialize, Clone)]
struct SquadsLimitedQuantityConstraints {
    max_per_period: u64,
}

#[derive(BorshSerialize)]
struct SquadsSpendingLimitPolicyCreationPayload {
    mint: Pubkey,
    source_account_index: u8,
    time_constraints: SquadsTimeConstraints,
    quantity_constraints: SquadsQuantityConstraints,
    usage_state: Option<SquadsUsageState>,
    destinations: Vec<Pubkey>,
}

#[derive(BorshSerialize)]
struct SquadsTimeConstraints {
    start: i64,
    expiration: Option<i64>,
    period: SquadsPeriodV2,
    accumulate_unused: bool,
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
enum SquadsPeriodV2 {
    OneTime,
    Daily,
    Weekly,
    Monthly,
    Custom(i64),
}

#[derive(BorshSerialize)]
struct SquadsQuantityConstraints {
    max_per_period: u64,
    max_per_use: u64,
    enforce_exact_quantity: bool,
}

#[derive(BorshSerialize)]
struct SquadsUsageState {
    remaining_in_period: u64,
    last_reset: i64,
}

#[derive(BorshSerialize)]
struct SquadsSyncSettingsTransactionArgs {
    num_signers: u8,
    actions: Vec<SquadsSettingsAction>,
    memo: Option<String>,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(SquadsPolicyPayload),
}

#[derive(BorshSerialize)]
struct SquadsSyncTransactionArgs {
    account_index: u8,
    num_signers: u8,
    payload: SquadsSyncPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyPayload {
    InternalFundTransfer(Vec<u8>),
    ProgramInteraction(SquadsProgramInteractionPayload),
    SpendingLimit(SquadsSpendingLimitPayload),
    SettingsChange(Vec<u8>),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPayload {
    instruction_constraint_indices: Option<Vec<u8>>,
    transaction_payload: SquadsProgramInteractionTransactionPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsProgramInteractionTransactionPayload {
    AsyncTransaction(Vec<u8>),
    SyncTransaction(SquadsProgramInteractionSyncPayload),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionSyncPayload {
    account_index: u8,
    instructions: Vec<u8>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MockJupiterTokenAccounts {
    pub authority: Pubkey,
    pub usdc_reserve: Pubkey,
    pub pyusd_reserve: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MockKaminoReserveTokenAccounts {
    pub reserve: Pubkey,
    pub market: Pubkey,
    pub liquidity_mint: Pubkey,
    pub collateral_mint: Pubkey,
    pub reserve_liquidity_authority: Pubkey,
    pub collateral_mint_authority: Pubkey,
    pub vault_liquidity: Pubkey,
    pub vault_collateral: Pubkey,
    pub reserve_liquidity_supply: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MockJupiterStableReserveTokenAccount {
    pub mint: Pubkey,
    pub reserve: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsYieldRoutePolicyWhitelist {
    pub stable_mints: Vec<Pubkey>,
    pub kamino_reserves: Vec<MockKaminoReserveTokenAccounts>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SwapLane {
    Jupiter,
    LoyalHub {
        hub_authorizer: Pubkey,
        max_fee_bps: u16,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SquadsYieldRoutePolicySeeds {
    pub withdraw: u64,
    pub swap: u64,
    pub deposit: u64,
}

impl Default for SquadsYieldRoutePolicySeeds {
    fn default() -> Self {
        Self {
            withdraw: YIELD_ROUTE_WITHDRAW_POLICY_SEED,
            swap: YIELD_ROUTE_SWAP_POLICY_SEED,
            deposit: YIELD_ROUTE_DEPOSIT_POLICY_SEED,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SquadsYieldRoutePolicies {
    pub withdraw: Pubkey,
    pub swap: Pubkey,
    pub deposit: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsYieldRoutePolicyInstructions {
    pub policies: SquadsYieldRoutePolicies,
    pub instructions: Vec<Instruction>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsYieldRoutePolicyInstruction {
    pub policy: Pubkey,
    pub instruction: Instruction,
}

#[derive(BorshSerialize)]
struct SquadsSpendingLimitPayload {
    amount: u64,
    destination: Pubkey,
    decimals: u8,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SquadsPool {
    pub seed: u128,
    pub settings: Pubkey,
    pub settings_bump: u8,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FundedSquadsTestConfig {
    pub smart_account_seed: u128,
    pub vault_index: u8,
    pub wallet_airdrop_lamports: u64,
    pub vault_funding_lamports: u64,
}

impl Default for FundedSquadsTestConfig {
    fn default() -> Self {
        Self {
            smart_account_seed: 1,
            vault_index: 0,
            wallet_airdrop_lamports: DEFAULT_WALLET_AIRDROP_LAMPORTS,
            vault_funding_lamports: DEFAULT_WALLET_AIRDROP_LAMPORTS / 2,
        }
    }
}

pub struct FundedSquadsTestContext {
    pub svm: LiteSVM,
    pub wallet: Keypair,
    pub pool: SquadsPool,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub wallet_airdrop_lamports: u64,
    pub vault_funding_lamports: u64,
    pub loaded_program_path: PathBuf,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MockProgram {
    Jupiter,
    KaminoLend,
    LoyalHubSwap,
}

impl FundedSquadsTestContext {
    pub fn wallet_pubkey(&self) -> Pubkey {
        self.wallet.pubkey()
    }

    pub fn wallet_balance(&self) -> u64 {
        self.svm
            .get_account(&self.wallet.pubkey())
            .map(|account| account.lamports)
            .unwrap_or(0)
    }

    pub fn vault_balance(&self) -> u64 {
        self.svm
            .get_account(&self.vault)
            .map(|account| account.lamports)
            .unwrap_or(0)
    }

    pub fn sync_transfer_from_vault_to_wallet(&mut self, lamports: u64) {
        let instruction = execute_squads_sync_transfer_instruction(
            self.pool.settings,
            self.wallet.pubkey(),
            self.vault_index,
            self.wallet.pubkey(),
            lamports,
        );
        send_instructions(&mut self.svm, &[instruction], &self.wallet);
    }
}

pub fn create_squads_yield_route_policy_instructions(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_policy_instructions_with_swap_lanes(
        context,
        delegated_signer,
        whitelist,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_policy_instructions_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstructions {
    create_squads_yield_route_policy_instructions_with_seeds(
        context,
        delegated_signer,
        whitelist,
        swap_lanes,
        SquadsYieldRoutePolicySeeds::default(),
    )
}

pub fn create_squads_yield_route_policy_instructions_with_seeds(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    whitelist: SquadsYieldRoutePolicyWhitelist,
    swap_lanes: Vec<SwapLane>,
    seeds: SquadsYieldRoutePolicySeeds,
) -> SquadsYieldRoutePolicyInstructions {
    let settings = context.pool.settings;
    let authority = context.wallet_pubkey();
    let account_index = context.vault_index;
    let vault = context.vault;
    let stable_mints = unique_pubkeys(whitelist.stable_mints);
    let kamino_reserves = unique_kamino_reserves(whitelist.kamino_reserves);
    let (withdraw, _) = derive_squads_policy(&settings, seeds.withdraw);
    let (swap, _) = derive_squads_policy(&settings, seeds.swap);
    let (deposit, _) = derive_squads_policy(&settings, seeds.deposit);

    SquadsYieldRoutePolicyInstructions {
        policies: SquadsYieldRoutePolicies {
            withdraw,
            swap,
            deposit,
        },
        instructions: vec![
            create_squads_program_interaction_route_kamino_withdraw_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seeds.withdraw,
                account_index,
                vault,
                kamino_reserves.clone(),
            ),
            create_squads_program_interaction_route_stable_swap_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seeds.swap,
                account_index,
                vault,
                stable_mints,
                swap_lanes,
            ),
            create_squads_program_interaction_route_kamino_deposit_policy_instruction(
                settings,
                authority,
                delegated_signer,
                seeds.deposit,
                account_index,
                vault,
                kamino_reserves,
            ),
        ],
    }
}

pub fn create_squads_yield_route_swap_policy_instruction(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    stable_mints: Vec<Pubkey>,
) -> SquadsYieldRoutePolicyInstruction {
    create_squads_yield_route_swap_policy_instruction_with_swap_lanes(
        context,
        delegated_signer,
        stable_mints,
        vec![SwapLane::Jupiter],
    )
}

pub fn create_squads_yield_route_swap_policy_instruction_with_swap_lanes(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    stable_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
) -> SquadsYieldRoutePolicyInstruction {
    create_squads_yield_route_swap_policy_instruction_with_seed(
        context,
        delegated_signer,
        stable_mints,
        swap_lanes,
        YIELD_ROUTE_STANDALONE_POLICY_SEED,
    )
}

pub fn create_squads_yield_route_swap_policy_instruction_with_seed(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
    stable_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
    policy_seed: u64,
) -> SquadsYieldRoutePolicyInstruction {
    let settings = context.pool.settings;
    let (policy, _) = derive_squads_policy(&settings, policy_seed);

    SquadsYieldRoutePolicyInstruction {
        policy,
        instruction: create_squads_program_interaction_route_stable_swap_policy_instruction(
            settings,
            context.wallet_pubkey(),
            delegated_signer,
            policy_seed,
            context.vault_index,
            context.vault,
            unique_pubkeys(stable_mints),
            swap_lanes,
        ),
    }
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

fn unique_kamino_reserves(
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Vec<MockKaminoReserveTokenAccounts> {
    let mut unique = Vec::new();
    for reserve in reserves {
        if !unique
            .iter()
            .any(|existing: &MockKaminoReserveTokenAccounts| {
                existing.reserve == reserve.reserve && existing.market == reserve.market
            })
        {
            unique.push(reserve);
        }
    }
    unique
}

pub fn new_litesvm() -> LiteSVM {
    LiteSVM::new()
}

pub fn derive_squads_settings(seed: u128) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_SETTINGS,
            &seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_pool(seed: u128) -> SquadsPool {
    let (settings, settings_bump) = derive_squads_settings(seed);
    SquadsPool {
        seed,
        settings,
        settings_bump,
    }
}

pub fn derive_squads_vault(squads_settings: &Pubkey, vault_index: u8) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            squads_settings.as_ref(),
            SQUADS_SEED_SMART_ACCOUNT,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_policy(squads_settings: &Pubkey, policy_seed: u64) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_POLICY,
            squads_settings.as_ref(),
            &policy_seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_program_config() -> Pubkey {
    Pubkey::find_program_address(
        &[SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

pub fn anchor_instruction_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("global:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().unwrap()
}

pub fn squads_test_treasury() -> Pubkey {
    Pubkey::new_from_array(hash32(b"loyal-yield-routing-squads-treasury"))
}

pub fn serialize_squads_program_config(
    authority: Pubkey,
    treasury: Pubkey,
    smart_account_index: u128,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(160);
    data.extend_from_slice(&SQUADS_PROGRAM_CONFIG_DISCRIMINATOR);
    smart_account_index.serialize(&mut data).unwrap();
    authority.serialize(&mut data).unwrap();
    0u64.serialize(&mut data).unwrap();
    treasury.serialize(&mut data).unwrap();
    [0u8; 64].serialize(&mut data).unwrap();
    data
}

pub fn seed_squads_program_config(
    svm: &mut LiteSVM,
    authority: Pubkey,
    treasury: Pubkey,
    smart_account_index: u128,
) -> Pubkey {
    let program_config = derive_squads_program_config();
    let data = serialize_squads_program_config(authority, treasury, smart_account_index);

    svm.set_account(
        program_config,
        Account {
            lamports: 1_000_000_000,
            data,
            owner: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed Squads program config account");

    program_config
}

pub fn serialize_squads_create_smart_account_args(verifier: Pubkey) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR);
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    1u16.serialize(&mut data).unwrap();
    1u32.serialize(&mut data).unwrap();
    verifier.serialize(&mut data).unwrap();
    SQUADS_FULL_PERMISSIONS_MASK.serialize(&mut data).unwrap();
    0u32.serialize(&mut data).unwrap();
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    Option::<String>::None.serialize(&mut data).unwrap();
    data
}

pub fn create_squads_smart_account_instruction(
    payer: Pubkey,
    verifier: Pubkey,
    seed: u128,
) -> Instruction {
    assert!(seed > 0, "Squads smart-account seed starts at 1");
    let program_config = derive_squads_program_config();
    let treasury = squads_test_treasury();
    let (settings, _) = derive_squads_settings(seed);

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
        data: serialize_squads_create_smart_account_args(verifier),
    }
}

pub fn squads_system_transfer_payload(lamports: u64) -> Vec<u8> {
    let mut transfer_data = system_transfer_data(lamports);
    let mut payload = Vec::with_capacity(7 + transfer_data.len());
    payload.push(1);
    payload.push(2);
    payload.push(2);
    payload.push(0);
    payload.push(1);
    payload.extend_from_slice(&(transfer_data.len() as u16).to_le_bytes());
    payload.append(&mut transfer_data);
    payload
}

pub fn system_transfer_data(lamports: u64) -> Vec<u8> {
    system_instruction::transfer(&Pubkey::default(), &Pubkey::default(), lamports).data
}

#[derive(Debug)]
pub struct SquadsCompiledInstruction {
    pub program_id_index: usize,
    pub accounts: Vec<usize>,
    pub data: Vec<u8>,
}

pub fn squads_compiled_instruction_payload(instructions: &[SquadsCompiledInstruction]) -> Vec<u8> {
    let mut payload = Vec::new();
    payload.push(
        instructions
            .len()
            .try_into()
            .expect("Squads sync payload supports up to 255 instructions"),
    );

    for instruction in instructions {
        payload.push(
            instruction
                .program_id_index
                .try_into()
                .expect("program id index fits in u8"),
        );
        payload.push(
            instruction
                .accounts
                .len()
                .try_into()
                .expect("account index count fits in u8"),
        );
        for account in &instruction.accounts {
            payload.push(
                (*account)
                    .try_into()
                    .expect("account index fits in Squads u8 account index"),
            );
        }
        payload.extend_from_slice(&(instruction.data.len() as u16).to_le_bytes());
        payload.extend_from_slice(&instruction.data);
    }

    payload
}

pub fn mock_jupiter_swap_data(
    operation: u8,
    amount: u64,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(73);
    data.push(operation);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(input_mint.as_ref());
    data.extend_from_slice(output_mint.as_ref());
    data
}

pub fn mock_jupiter_stable_exact_in_swap_data(
    in_amount: u64,
    out_amount: u64,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(81);
    data.push(MOCK_JUPITER_STABLE_EXACT_IN);
    data.extend_from_slice(&in_amount.to_le_bytes());
    data.extend_from_slice(&out_amount.to_le_bytes());
    data.extend_from_slice(input_mint.as_ref());
    data.extend_from_slice(output_mint.as_ref());
    data
}

pub fn loyal_hub_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Vec<u8> {
    assert!(
        !allowed_mints.is_empty() && allowed_mints.len() <= 8,
        "Loyal Hub supports 1..=8 allowed mints"
    );
    let mut data = Vec::with_capacity(68 + (allowed_mints.len() * 32));
    data.extend_from_slice(admin.as_ref());
    data.extend_from_slice(hub_authorizer.as_ref());
    data.extend_from_slice(&max_fee_bps.to_le_bytes());
    data.push(u8::from(paused));
    data.push(
        allowed_mints
            .len()
            .try_into()
            .expect("allowed mint count fits in u8"),
    );
    for mint in allowed_mints {
        data.extend_from_slice(mint.as_ref());
    }
    data
}

pub fn loyal_hub_initialize_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Vec<u8> {
    let mut data = vec![LOYAL_HUB_INITIALIZE_CONFIG];
    data.extend_from_slice(&loyal_hub_config_data(
        admin,
        hub_authorizer,
        max_fee_bps,
        paused,
        allowed_mints,
    ));
    data
}

pub fn loyal_hub_set_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Vec<u8> {
    let mut data = vec![LOYAL_HUB_SET_CONFIG];
    data.extend_from_slice(&loyal_hub_config_data(
        admin,
        hub_authorizer,
        max_fee_bps,
        paused,
        allowed_mints,
    ));
    data
}

pub fn loyal_hub_swap_exact_in_data(
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(27);
    data.push(LOYAL_HUB_SWAP_EXACT_IN);
    data.extend_from_slice(&amount_in.to_le_bytes());
    data.extend_from_slice(&amount_out.to_le_bytes());
    data.extend_from_slice(&min_out.to_le_bytes());
    data.extend_from_slice(&max_fee_bps.to_le_bytes());
    data
}

pub fn loyal_hub_set_paused_data(paused: bool) -> Vec<u8> {
    vec![LOYAL_HUB_SET_PAUSED, u8::from(paused)]
}

pub fn loyal_hub_withdraw_inventory_data(amount: u64) -> Vec<u8> {
    let mut data = Vec::with_capacity(9);
    data.push(LOYAL_HUB_WITHDRAW_INVENTORY);
    data.extend_from_slice(&amount.to_le_bytes());
    data
}

pub fn mock_kamino_deposit_reserve_liquidity_data(amount: u64) -> Vec<u8> {
    mock_kamino_reserve_liquidity_data(KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, amount)
}

pub fn mock_kamino_withdraw_reserve_liquidity_data(amount: u64) -> Vec<u8> {
    mock_kamino_reserve_liquidity_data(KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR, amount)
}

fn mock_kamino_reserve_liquidity_data(discriminator: [u8; 8], amount: u64) -> Vec<u8> {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&discriminator);
    data.extend_from_slice(&amount.to_le_bytes());
    data
}

pub fn mock_kamino_reserve_transaction(
    vault: Pubkey,
    reserve_accounts: MockKaminoReserveTokenAccounts,
    data: Vec<u8>,
) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
    let transaction_accounts = vec![
        AccountMeta::new(vault, false),
        AccountMeta::new(reserve_accounts.reserve, false),
        AccountMeta::new_readonly(reserve_accounts.market, false),
        AccountMeta::new_readonly(reserve_accounts.liquidity_mint, false),
        AccountMeta::new(reserve_accounts.vault_liquidity, false),
        AccountMeta::new(reserve_accounts.vault_collateral, false),
        AccountMeta::new(reserve_accounts.reserve_liquidity_supply, false),
        AccountMeta::new(reserve_accounts.collateral_mint, false),
        AccountMeta::new_readonly(reserve_accounts.reserve_liquidity_authority, false),
        AccountMeta::new_readonly(reserve_accounts.collateral_mint_authority, false),
        AccountMeta::new_readonly(spl_token::id(), false),
        AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
    ];

    (
        vec![SquadsCompiledInstruction {
            program_id_index: 11,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
            data,
        }],
        transaction_accounts,
    )
}

pub fn derive_mock_jupiter_swap_authority() -> Pubkey {
    Pubkey::find_program_address(&[JUPITER_SWAP_AUTHORITY_SEED], &JUPITER_V6_PROGRAM_ID).0
}

pub fn derive_loyal_hub_config() -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_CONFIG_SEED], &LOYAL_HUB_SWAP_PROGRAM_ID).0
}

pub fn derive_loyal_hub_authority() -> Pubkey {
    Pubkey::find_program_address(&[LOYAL_HUB_AUTHORITY_SEED], &LOYAL_HUB_SWAP_PROGRAM_ID).0
}

pub fn loyal_hub_token_account(mint: Pubkey) -> Pubkey {
    Pubkey::new_from_array(hashv(&[LOYAL_HUB_TOKEN_ACCOUNT_SEED, mint.as_ref()]).to_bytes())
}

pub fn mock_jupiter_usdc_reserve_token_account() -> Pubkey {
    Pubkey::new_from_array(hash32(MOCK_JUPITER_USDC_RESERVE_TOKEN_ACCOUNT_SEED))
}

pub fn mock_jupiter_pyusd_reserve_token_account() -> Pubkey {
    Pubkey::new_from_array(hash32(MOCK_JUPITER_PYUSD_RESERVE_TOKEN_ACCOUNT_SEED))
}

pub fn mock_jupiter_stable_reserve_token_account(mint: Pubkey) -> Pubkey {
    Pubkey::new_from_array(
        hashv(&[
            MOCK_JUPITER_STABLE_RESERVE_TOKEN_ACCOUNT_SEED,
            mint.as_ref(),
        ])
        .to_bytes(),
    )
}

pub fn mock_jupiter_token_accounts() -> MockJupiterTokenAccounts {
    MockJupiterTokenAccounts {
        authority: derive_mock_jupiter_swap_authority(),
        usdc_reserve: mock_jupiter_usdc_reserve_token_account(),
        pyusd_reserve: mock_jupiter_pyusd_reserve_token_account(),
    }
}

pub fn derive_mock_kamino_reserve_liquidity_authority(reserve: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED, reserve.as_ref()],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn derive_mock_kamino_collateral_mint_authority(reserve: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[KAMINO_COLLATERAL_MINT_AUTHORITY_SEED, reserve.as_ref()],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn mock_kamino_collateral_mint(reserve: Pubkey) -> Pubkey {
    Pubkey::new_from_array(hashv(&[b"mock-kamino-collateral-mint", reserve.as_ref()]).to_bytes())
}

pub fn seed_empty_system_account_if_missing(svm: &mut LiteSVM, pubkey: Pubkey) {
    if svm.get_account(&pubkey).is_some() {
        return;
    }

    svm.set_account(
        pubkey,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data: vec![],
            owner: solana_sdk::system_program::ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed empty system account");
}

pub fn seed_spl_mint(
    svm: &mut LiteSVM,
    mint: Pubkey,
    mint_authority: Option<Pubkey>,
    decimals: u8,
    supply: u64,
) {
    let mut data = vec![0; spl_token::state::Mint::LEN];
    spl_token::state::Mint {
        mint_authority: mint_authority.map_or(COption::None, COption::Some),
        supply,
        decimals,
        is_initialized: true,
        freeze_authority: COption::None,
    }
    .pack_into_slice(&mut data);

    svm.set_account(
        mint,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data,
            owner: spl_token::id(),
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed SPL mint");
}

pub fn seed_spl_mint_if_missing(
    svm: &mut LiteSVM,
    mint: Pubkey,
    mint_authority: Option<Pubkey>,
    decimals: u8,
    supply: u64,
) {
    if svm.get_account(&mint).is_none() {
        seed_spl_mint(svm, mint, mint_authority, decimals, supply);
    }
}

pub fn seed_spl_token_account(
    svm: &mut LiteSVM,
    token_account: Pubkey,
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
) {
    let mut data = vec![0; spl_token::state::Account::LEN];
    spl_token::state::Account {
        mint,
        owner,
        amount,
        delegate: COption::None,
        state: spl_token::state::AccountState::Initialized,
        is_native: COption::None,
        delegated_amount: 0,
        close_authority: COption::None,
    }
    .pack_into_slice(&mut data);

    svm.set_account(
        token_account,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data,
            owner: spl_token::id(),
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed SPL token account");
}

pub fn seed_spl_token_account_if_missing(
    svm: &mut LiteSVM,
    token_account: Pubkey,
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
) {
    if svm.get_account(&token_account).is_none() {
        seed_spl_token_account(svm, token_account, mint, owner, amount);
    }
}

pub fn get_spl_token_amount(svm: &LiteSVM, token_account: Pubkey) -> u64 {
    let account = svm
        .get_account(&token_account)
        .expect("SPL token account exists");
    let token_account =
        spl_token::state::Account::unpack(&account.data).expect("unpack SPL token account");
    token_account.amount
}

pub fn set_spl_token_amount(svm: &mut LiteSVM, token_account: Pubkey, amount: u64) {
    let mut account = svm
        .get_account(&token_account)
        .expect("SPL token account exists");
    let mut token =
        spl_token::state::Account::unpack(&account.data).expect("unpack SPL token account");
    token.amount = amount;
    token.pack_into_slice(&mut account.data);
    svm.set_account(token_account, account)
        .expect("update SPL token account amount");
}

pub fn set_spl_mint_supply(svm: &mut LiteSVM, mint: Pubkey, supply: u64) {
    let mut account = svm.get_account(&mint).expect("SPL mint exists");
    let mut mint_state = spl_token::state::Mint::unpack(&account.data).expect("unpack SPL mint");
    mint_state.supply = supply;
    mint_state.pack_into_slice(&mut account.data);
    svm.set_account(mint, account)
        .expect("update SPL mint supply");
}

pub fn seed_mock_jupiter_spl_accounts(
    svm: &mut LiteSVM,
    usdc_reserve_amount: u64,
    pyusd_reserve_amount: u64,
) -> MockJupiterTokenAccounts {
    let accounts = mock_jupiter_token_accounts();
    seed_empty_system_account_if_missing(svm, accounts.authority);
    seed_spl_mint_if_missing(svm, USDC_MINT, None, USDC_DECIMALS, usdc_reserve_amount);
    seed_spl_mint_if_missing(svm, PYUSD_MINT, None, PYUSD_DECIMALS, pyusd_reserve_amount);
    seed_spl_token_account(
        svm,
        accounts.usdc_reserve,
        USDC_MINT,
        accounts.authority,
        usdc_reserve_amount,
    );
    seed_spl_token_account(
        svm,
        accounts.pyusd_reserve,
        PYUSD_MINT,
        accounts.authority,
        pyusd_reserve_amount,
    );
    accounts
}

pub fn seed_mock_jupiter_stable_reserve_spl_accounts(
    svm: &mut LiteSVM,
    stable_reserves: &[MockJupiterStableReserveTokenAccount],
    reserve_amount: u64,
) {
    let authority = derive_mock_jupiter_swap_authority();
    seed_empty_system_account_if_missing(svm, authority);
    for stable_reserve in stable_reserves {
        seed_spl_token_account(
            svm,
            stable_reserve.reserve,
            stable_reserve.mint,
            authority,
            reserve_amount,
        );
    }
}

pub fn seed_loyal_hub_inventory_spl_accounts(
    svm: &mut LiteSVM,
    stable_mints: &[Pubkey],
    reserve_amount: u64,
) -> Vec<Pubkey> {
    let authority = derive_loyal_hub_authority();
    seed_empty_system_account_if_missing(svm, authority);
    stable_mints
        .iter()
        .map(|mint| {
            let token_account = loyal_hub_token_account(*mint);
            seed_spl_token_account(svm, token_account, *mint, authority, reserve_amount);
            token_account
        })
        .collect()
}

pub fn seed_mock_kamino_reserve_spl_accounts(
    svm: &mut LiteSVM,
    reserve: Pubkey,
    market: Pubkey,
    vault: Pubkey,
    vault_liquidity: Pubkey,
    vault_collateral: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> MockKaminoReserveTokenAccounts {
    seed_mock_kamino_reserve_spl_accounts_with_mint(
        svm,
        reserve,
        market,
        USDC_MINT,
        USDC_DECIMALS,
        vault,
        vault_liquidity,
        vault_collateral,
        reserve_liquidity_supply,
    )
}

pub fn seed_mock_kamino_reserve_spl_accounts_with_mint(
    svm: &mut LiteSVM,
    reserve: Pubkey,
    market: Pubkey,
    liquidity_mint: Pubkey,
    liquidity_decimals: u8,
    vault: Pubkey,
    vault_liquidity: Pubkey,
    vault_collateral: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> MockKaminoReserveTokenAccounts {
    let reserve_liquidity_authority = derive_mock_kamino_reserve_liquidity_authority(reserve);
    let collateral_mint_authority = derive_mock_kamino_collateral_mint_authority(reserve);
    let collateral_mint = mock_kamino_collateral_mint(reserve);

    seed_empty_system_account_if_missing(svm, market);
    seed_empty_system_account_if_missing(svm, reserve);
    seed_empty_system_account_if_missing(svm, reserve_liquidity_authority);
    seed_empty_system_account_if_missing(svm, collateral_mint_authority);
    seed_spl_mint_if_missing(svm, liquidity_mint, None, liquidity_decimals, 0);
    seed_spl_mint(
        svm,
        collateral_mint,
        Some(collateral_mint_authority),
        KAMINO_COLLATERAL_DECIMALS,
        0,
    );
    seed_spl_token_account_if_missing(svm, vault_liquidity, liquidity_mint, vault, 0);
    seed_spl_token_account_if_missing(svm, vault_collateral, collateral_mint, vault, 0);
    seed_spl_token_account_if_missing(
        svm,
        reserve_liquidity_supply,
        liquidity_mint,
        reserve_liquidity_authority,
        0,
    );

    MockKaminoReserveTokenAccounts {
        reserve,
        market,
        liquidity_mint,
        collateral_mint,
        reserve_liquidity_authority,
        collateral_mint_authority,
        vault_liquidity,
        vault_collateral,
        reserve_liquidity_supply,
    }
}

pub fn add_mock_jupiter_program(svm: &mut LiteSVM) -> std::io::Result<PathBuf> {
    add_mock_yield_protocols_program(svm, JUPITER_V6_PROGRAM_ID)
}

pub fn add_mock_kamino_lend_program(svm: &mut LiteSVM) -> std::io::Result<PathBuf> {
    add_mock_yield_protocols_program(svm, KAMINO_LEND_PROGRAM_ID)
}

pub fn add_loyal_hub_swap_program(svm: &mut LiteSVM) -> std::io::Result<PathBuf> {
    let path = loyal_hub_swap_program_so_path().ok_or_else(|| {
        std::io::Error::new(
            std::io::ErrorKind::NotFound,
            format!(
                "Loyal Hub swap SBF program not found; run `cargo build-sbf -- -p loyal-hub-swap-program` or set {LOYAL_HUB_SWAP_PROGRAM_SO_ENV}"
            ),
        )
    })?;
    let program = fs::read(&path)?;
    svm.add_program(LOYAL_HUB_SWAP_PROGRAM_ID, &program)
        .map_err(|error| {
            std::io::Error::other(format!("add Loyal Hub swap program failed: {error}"))
        })?;
    Ok(path)
}

pub fn add_mock_yield_protocols_program(
    svm: &mut LiteSVM,
    program_id: Pubkey,
) -> std::io::Result<PathBuf> {
    let path = mock_yield_protocols_program_so_path().ok_or_else(|| {
        std::io::Error::new(
            std::io::ErrorKind::NotFound,
            format!(
                "mock yield protocols SBF program not found; run `cargo build-sbf -- -p mock-yield-protocols-program` or set {MOCK_YIELD_PROTOCOLS_PROGRAM_SO_ENV}"
            ),
        )
    })?;
    let program = fs::read(&path)?;
    svm.add_program(program_id, &program).map_err(|error| {
        std::io::Error::other(format!("add mock yield protocols program failed: {error}"))
    })?;
    Ok(path)
}

pub fn mock_yield_protocols_program_so_path() -> Option<PathBuf> {
    if let Some(path) = env::var_os(MOCK_YIELD_PROTOCOLS_PROGRAM_SO_ENV).map(PathBuf::from) {
        if path.exists() {
            return Some(path);
        }
    }

    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    for path in [
        manifest_dir
            .join("../../target/deploy")
            .join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
        PathBuf::from("target/deploy").join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
    ] {
        if path.exists() {
            return Some(path);
        }
    }

    None
}

pub fn loyal_hub_swap_program_so_path() -> Option<PathBuf> {
    if let Some(path) = env::var_os(LOYAL_HUB_SWAP_PROGRAM_SO_ENV).map(PathBuf::from) {
        if path.exists() {
            return Some(path);
        }
    }

    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    for path in [
        manifest_dir
            .join("../../target/deploy")
            .join(LOYAL_HUB_SWAP_PROGRAM_SO),
        PathBuf::from("target/deploy").join(LOYAL_HUB_SWAP_PROGRAM_SO),
    ] {
        if path.exists() {
            return Some(path);
        }
    }

    None
}

pub fn serialize_squads_sync_transaction_args(account_index: u8, payload: Vec<u8>) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    account_index.serialize(&mut data).unwrap();
    SQUADS_SYNC_SIGNER_COUNT.serialize(&mut data).unwrap();
    0u8.serialize(&mut data).unwrap();
    payload.serialize(&mut data).unwrap();
    data
}

fn serialize_squads_sync_settings_transaction_args(actions: Vec<SquadsSettingsAction>) -> Vec<u8> {
    let mut data = Vec::from(anchor_instruction_discriminator(
        "execute_settings_transaction_sync",
    ));
    SquadsSyncSettingsTransactionArgs {
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        actions,
        memo: None,
    }
    .serialize(&mut data)
    .unwrap();
    data
}

fn serialize_squads_sync_policy_payload_args(
    account_index: u8,
    policy_payload: SquadsPolicyPayload,
) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Policy(policy_payload),
    }
    .serialize(&mut data)
    .unwrap();
    data
}

pub fn execute_squads_sync_transfer_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    recipient: Pubkey,
    lamports: u64,
) -> Instruction {
    let (vault, _) = derive_squads_vault(&squads_settings, account_index);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(vault, false),
            AccountMeta::new(recipient, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: serialize_squads_sync_transaction_args(
            account_index,
            squads_system_transfer_payload(lamports),
        ),
    }
}

pub fn execute_squads_sync_transaction_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(squads_settings, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_transaction_args(
            account_index,
            squads_compiled_instruction_payload(&compiled_instructions),
        ),
    }
}

pub fn execute_mock_jupiter_sol_to_usdc_swap_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    jupiter_sol_escrow: Pubkey,
    amount: u64,
) -> Instruction {
    let jupiter_accounts = mock_jupiter_token_accounts();
    execute_squads_sync_transaction_instruction(
        squads_settings,
        signer,
        account_index,
        vec![
            SquadsCompiledInstruction {
                program_id_index: 2,
                accounts: vec![0, 1],
                data: system_transfer_data(amount),
            },
            SquadsCompiledInstruction {
                program_id_index: 3,
                accounts: vec![0, 4, 5, 6, 7, 8],
                data: mock_jupiter_swap_data(
                    MOCK_JUPITER_SOL_TO_USDC,
                    amount,
                    WRAPPED_SOL_MINT,
                    USDC_MINT,
                ),
            },
        ],
        vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(jupiter_sol_escrow, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
            AccountMeta::new(vault_usdc_token_account, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new(jupiter_accounts.usdc_reserve, false),
            AccountMeta::new_readonly(jupiter_accounts.authority, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
    )
}

pub fn execute_squads_yield_route_stable_swap_instruction(
    swap_policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    in_amount: u64,
    out_amount: u64,
) -> Instruction {
    execute_squads_program_interaction_instruction(
        swap_policy,
        signer,
        account_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 9,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8],
            data: mock_jupiter_stable_exact_in_swap_data(
                in_amount,
                out_amount,
                input_mint,
                output_mint,
            ),
        }],
        vec![0],
        vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(vault_input, false),
            AccountMeta::new(vault_output, false),
            AccountMeta::new_readonly(input_mint, false),
            AccountMeta::new_readonly(output_mint, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new(mock_jupiter_stable_reserve_token_account(input_mint), false),
            AccountMeta::new(
                mock_jupiter_stable_reserve_token_account(output_mint),
                false,
            ),
            AccountMeta::new_readonly(derive_mock_jupiter_swap_authority(), false),
            AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
        ],
    )
}

pub fn initialize_loyal_hub_config_instruction(
    payer: Pubkey,
    admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Instruction {
    assert_eq!(
        payer, admin,
        "test helper initializes config with the admin as payer"
    );
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(payer, true),
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: loyal_hub_initialize_config_data(
            admin,
            hub_authorizer,
            max_fee_bps,
            paused,
            allowed_mints,
        ),
    }
}

pub fn set_loyal_hub_paused_instruction(admin: Pubkey, paused: bool) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(admin, true),
        ],
        data: loyal_hub_set_paused_data(paused),
    }
}

pub fn set_loyal_hub_config_instruction(
    admin_signer: Pubkey,
    new_admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(admin_signer, true),
        ],
        data: loyal_hub_set_config_data(
            new_admin,
            hub_authorizer,
            max_fee_bps,
            paused,
            allowed_mints,
        ),
    }
}

pub fn withdraw_loyal_hub_inventory_instruction(
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new(hub_source, false),
            AccountMeta::new(destination, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: loyal_hub_withdraw_inventory_data(amount),
    }
}

pub fn execute_squads_yield_route_loyal_hub_swap_instruction(
    swap_policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
) -> Instruction {
    execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        swap_policy,
        signer,
        account_index,
        vault,
        vault_input,
        vault_output,
        input_mint,
        output_mint,
        hub_authorizer,
        amount_in,
        amount_out,
        min_out,
        max_fee_bps,
        0,
    )
}

pub fn execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
    swap_policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
    instruction_constraint_index: u8,
) -> Instruction {
    execute_squads_program_interaction_instruction(
        swap_policy,
        signer,
        account_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 11,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
            data: loyal_hub_swap_exact_in_data(amount_in, amount_out, min_out, max_fee_bps),
        }],
        vec![instruction_constraint_index],
        vec![
            AccountMeta::new_readonly(derive_loyal_hub_config(), false),
            AccountMeta::new(vault, false),
            AccountMeta::new(vault_input, false),
            AccountMeta::new(vault_output, false),
            AccountMeta::new(loyal_hub_token_account(input_mint), false),
            AccountMeta::new(loyal_hub_token_account(output_mint), false),
            AccountMeta::new_readonly(input_mint, false),
            AccountMeta::new_readonly(output_mint, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(hub_authorizer, true),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
        ],
    )
}

pub fn create_squads_spending_limit_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    source_account_index: u8,
    destination: Pubkey,
    max_per_period_lamports: u64,
    max_per_use_lamports: u64,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::SpendingLimit(
            SquadsSpendingLimitPolicyCreationPayload {
                mint: Pubkey::default(),
                source_account_index,
                time_constraints: SquadsTimeConstraints {
                    start: 0,
                    expiration: None,
                    period: SquadsPeriodV2::OneTime,
                    accumulate_unused: false,
                },
                quantity_constraints: SquadsQuantityConstraints {
                    max_per_period: max_per_period_lamports,
                    max_per_use: max_per_use_lamports,
                    enforce_exact_quantity: false,
                },
                usage_state: None,
                destinations: vec![destination],
            },
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn create_squads_program_interaction_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    usdc_ledger: Pubkey,
    pyusd_ledger: Pubkey,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let jupiter_accounts = mock_jupiter_token_accounts();
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: vec![SquadsInstructionConstraint {
                    program_id: JUPITER_V6_PROGRAM_ID,
                    account_constraints: vec![
                        SquadsAccountConstraint {
                            account_index: 0,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                            owner: None,
                        },
                        SquadsAccountConstraint {
                            account_index: 1,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                usdc_ledger,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 2,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                pyusd_ledger,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 3,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                USDC_MINT,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 4,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                PYUSD_MINT,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 5,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                spl_token::id(),
                            ]),
                            owner: None,
                        },
                        SquadsAccountConstraint {
                            account_index: 6,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                jupiter_accounts.usdc_reserve,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 7,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                jupiter_accounts.pyusd_reserve,
                            ]),
                            owner: Some(spl_token::id()),
                        },
                        SquadsAccountConstraint {
                            account_index: 8,
                            account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                                jupiter_accounts.authority,
                            ]),
                            owner: None,
                        },
                    ],
                    data_constraints: vec![
                        SquadsDataConstraint {
                            data_offset: 0,
                            data_value: SquadsDataValue::U8(MOCK_JUPITER_USDC_TO_PYUSD),
                            operator: SquadsDataOperator::Equals,
                        },
                        SquadsDataConstraint {
                            data_offset: 9,
                            data_value: SquadsDataValue::U8Slice(USDC_MINT.to_bytes().to_vec()),
                            operator: SquadsDataOperator::Equals,
                        },
                        SquadsDataConstraint {
                            data_offset: 41,
                            data_value: SquadsDataValue::U8Slice(PYUSD_MINT.to_bytes().to_vec()),
                            operator: SquadsDataOperator::Equals,
                        },
                    ],
                }],
                pre_hook: None,
                post_hook: None,
                spending_limits: vec![],
            },
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn create_squads_program_interaction_jupiter_fixture_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    usdc_ledger: Pubkey,
    pyusd_ledger: Pubkey,
    swap_instruction_data: &[u8],
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![jupiter_fixture_swap_instruction_constraint(
            vault,
            usdc_ledger,
            pyusd_ledger,
            swap_instruction_data.to_vec(),
        )],
    )
}

fn jupiter_fixture_swap_instruction_constraint(
    vault: Pubkey,
    usdc_ledger: Pubkey,
    pyusd_ledger: Pubkey,
    swap_instruction_data: Vec<u8>,
) -> SquadsInstructionConstraint {
    let jupiter_accounts = mock_jupiter_token_accounts();

    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![usdc_ledger]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![pyusd_ledger]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![USDC_MINT]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![PYUSD_MINT]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    jupiter_accounts.usdc_reserve,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    jupiter_accounts.pyusd_reserve,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    jupiter_accounts.authority,
                ]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(swap_instruction_data),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub fn create_squads_program_interaction_allowed_jupiter_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_token_accounts: Vec<Pubkey>,
    stable_reserves: Vec<MockJupiterStableReserveTokenAccount>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![jupiter_allowed_stable_swap_instruction_constraint(
            vault,
            vault_token_accounts,
            stable_reserves,
        )],
    )
}

pub fn create_squads_program_interaction_route_jupiter_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![jupiter_route_stable_swap_instruction_constraint(
            vault,
            allowed_mints,
        )],
    )
}

pub fn create_squads_program_interaction_route_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
) -> Instruction {
    assert!(
        !swap_lanes.is_empty(),
        "yield route swap policy needs at least one swap lane"
    );
    let mut constraints = Vec::with_capacity(swap_lanes.len());
    for lane in swap_lanes {
        match lane {
            SwapLane::Jupiter => constraints.push(
                jupiter_route_stable_swap_instruction_constraint(vault, allowed_mints.clone()),
            ),
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => constraints.push(loyal_hub_route_stable_swap_instruction_constraint(
                vault,
                allowed_mints.clone(),
                hub_authorizer,
                max_fee_bps,
            )),
        }
    }

    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        constraints,
    )
}

pub fn create_squads_program_interaction_mock_jupiter_stable_swap_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![SquadsInstructionConstraint {
            program_id: JUPITER_V6_PROGRAM_ID,
            account_constraints: vec![
                SquadsAccountConstraint {
                    account_index: 0,
                    account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                    owner: None,
                },
                SquadsAccountConstraint {
                    account_index: 1,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 2,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 3,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 4,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 5,
                    account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                    owner: None,
                },
                SquadsAccountConstraint {
                    account_index: 6,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 7,
                    account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                    owner: Some(spl_token::id()),
                },
                SquadsAccountConstraint {
                    account_index: 8,
                    account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                        derive_mock_jupiter_swap_authority(),
                    ]),
                    owner: None,
                },
            ],
            data_constraints: vec![SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            }],
        }],
    )
}

fn jupiter_route_stable_swap_instruction_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_mock_jupiter_swap_authority(),
                ]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn loyal_hub_route_stable_swap_instruction_constraint(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_loyal_hub_config(),
                ]),
                owner: Some(LOYAL_HUB_SWAP_PROGRAM_ID),
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(allowed_mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_loyal_hub_authority(),
                ]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![hub_authorizer]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![
            SquadsDataConstraint {
                data_offset: 0,
                data_value: SquadsDataValue::U8(LOYAL_HUB_SWAP_EXACT_IN),
                operator: SquadsDataOperator::Equals,
            },
            SquadsDataConstraint {
                data_offset: 25,
                data_value: SquadsDataValue::U16Le(max_fee_bps),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
        ],
    }
}

fn jupiter_allowed_stable_swap_instruction_constraint(
    vault: Pubkey,
    vault_token_accounts: Vec<Pubkey>,
    stable_reserves: Vec<MockJupiterStableReserveTokenAccount>,
) -> SquadsInstructionConstraint {
    let mints = stable_reserves
        .iter()
        .map(|stable_reserve| stable_reserve.mint)
        .collect::<Vec<_>>();
    let reserve_token_accounts = stable_reserves
        .iter()
        .map(|stable_reserve| stable_reserve.reserve)
        .collect::<Vec<_>>();

    SquadsInstructionConstraint {
        program_id: JUPITER_V6_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    vault_token_accounts.clone(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(vault_token_accounts),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(mints.clone()),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(mints),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserve_token_accounts.clone(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(reserve_token_accounts),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    derive_mock_jupiter_swap_authority(),
                ]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8(MOCK_JUPITER_STABLE_EXACT_IN),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn kamino_reserve_instruction_constraint(
    reserve: Pubkey,
    market: Pubkey,
    liquidity_mint: Pubkey,
    data_constraint_value: Vec<u8>,
    vault: Pubkey,
    vault_liquidity_token_account: Pubkey,
    vault_collateral_token_account: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> SquadsInstructionConstraint {
    let collateral_mint = mock_kamino_collateral_mint(reserve);
    let reserve_liquidity_authority = derive_mock_kamino_reserve_liquidity_authority(reserve);
    let collateral_mint_authority = derive_mock_kamino_collateral_mint_authority(reserve);

    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![reserve]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![market]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![liquidity_mint]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    vault_liquidity_token_account,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    vault_collateral_token_account,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    reserve_liquidity_supply,
                ]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![collateral_mint]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    reserve_liquidity_authority,
                ]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                    collateral_mint_authority,
                ]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(data_constraint_value),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

pub fn create_squads_program_interaction_allowed_kamino_reserves_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_allowed_reserves_instruction_constraint(
                vault,
                &reserves,
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
            kamino_allowed_reserves_instruction_constraint(
                vault,
                &reserves,
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
        ],
    )
}

pub fn create_squads_program_interaction_route_kamino_withdraw_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![kamino_route_reserves_instruction_constraint(
            vault,
            &reserves,
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        )],
    )
}

pub fn create_squads_program_interaction_route_kamino_deposit_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> Instruction {
    create_squads_compact_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![kamino_route_reserves_instruction_constraint(
            vault,
            &reserves,
            KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        )],
    )
}

pub fn create_squads_program_interaction_mock_kamino_reserves_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            mock_kamino_reserve_instruction_constraint(
                vault,
                KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
            mock_kamino_reserve_instruction_constraint(
                vault,
                KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            ),
        ],
    )
}

fn kamino_route_reserves_instruction_constraint(
    vault: Pubkey,
    reserves: &[MockKaminoReserveTokenAccounts],
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.reserve).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.market).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn mock_kamino_reserve_instruction_constraint(
    vault: Pubkey,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::AccountData(vec![]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn kamino_allowed_reserves_instruction_constraint(
    vault: Pubkey,
    reserves: &[MockKaminoReserveTokenAccounts],
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            SquadsAccountConstraint {
                account_index: 0,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![vault]),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 1,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.reserve).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 2,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves.iter().map(|reserve| reserve.market).collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 3,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.liquidity_mint)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 4,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.vault_liquidity)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 5,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.vault_collateral)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 6,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.reserve_liquidity_supply)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 7,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.collateral_mint)
                        .collect(),
                ),
                owner: Some(spl_token::id()),
            },
            SquadsAccountConstraint {
                account_index: 8,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.reserve_liquidity_authority)
                        .collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 9,
                account_constraint: SquadsAccountConstraintType::Pubkey(
                    reserves
                        .iter()
                        .map(|reserve| reserve.collateral_mint_authority)
                        .collect(),
                ),
                owner: None,
            },
            SquadsAccountConstraint {
                account_index: 10,
                account_constraint: SquadsAccountConstraintType::Pubkey(vec![spl_token::id()]),
                owner: None,
            },
        ],
        data_constraints: vec![SquadsDataConstraint {
            data_offset: 0,
            data_value: SquadsDataValue::U8Slice(discriminator.to_vec()),
            operator: SquadsDataOperator::Equals,
        }],
    }
}

fn create_squads_compact_program_interaction_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::ProgramInteraction(
            compile_squads_program_interaction_policy_creation_payload(account_index, constraints),
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

fn compile_squads_program_interaction_policy_creation_payload(
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> SquadsProgramInteractionPolicyCreationPayload {
    let mut pubkey_table = Vec::new();
    let instructions_constraints = constraints
        .into_iter()
        .map(|constraint| compile_squads_instruction_constraint(constraint, &mut pubkey_table))
        .collect::<Vec<_>>();

    SquadsProgramInteractionPolicyCreationPayload {
        account_index,
        pubkey_table: pubkey_table.into(),
        instructions_constraints: instructions_constraints.into(),
        pre_hook: None,
        post_hook: None,
        spending_limits: Vec::<SquadsCompiledLimitedSpendingLimit>::new().into(),
    }
}

fn compile_squads_instruction_constraint(
    constraint: SquadsInstructionConstraint,
    pubkey_table: &mut Vec<Pubkey>,
) -> SquadsCompiledInstructionConstraint {
    SquadsCompiledInstructionConstraint {
        program_id_index: squads_pubkey_table_index(pubkey_table, constraint.program_id),
        account_constraints: constraint
            .account_constraints
            .into_iter()
            .map(|account_constraint| {
                compile_squads_account_constraint(account_constraint, pubkey_table)
            })
            .collect::<Vec<_>>()
            .into(),
        data_constraints: constraint.data_constraints.into(),
    }
}

fn compile_squads_account_constraint(
    constraint: SquadsAccountConstraint,
    pubkey_table: &mut Vec<Pubkey>,
) -> SquadsCompiledAccountConstraint {
    SquadsCompiledAccountConstraint {
        account_index: constraint.account_index,
        account_constraint: match constraint.account_constraint {
            SquadsAccountConstraintType::Pubkey(pubkeys) => {
                SquadsCompiledAccountConstraintType::Pubkey(
                    pubkeys
                        .into_iter()
                        .map(|pubkey| squads_pubkey_table_index(pubkey_table, pubkey))
                        .collect::<Vec<_>>()
                        .into(),
                )
            }
            SquadsAccountConstraintType::AccountData(data_constraints) => {
                SquadsCompiledAccountConstraintType::AccountData(data_constraints.into())
            }
        },
        owner_index: constraint
            .owner
            .map(|owner| squads_pubkey_table_index(pubkey_table, owner)),
    }
}

fn squads_pubkey_table_index(pubkey_table: &mut Vec<Pubkey>, pubkey: Pubkey) -> u8 {
    if let Some(index) = pubkey_table.iter().position(|existing| *existing == pubkey) {
        return index.try_into().expect("pubkey table index fits in u8");
    }

    assert!(
        pubkey_table.len() < 240,
        "Squads ProgramInteraction pubkey table supports up to 240 custom pubkeys"
    );
    let index = pubkey_table.len();
    pubkey_table.push(pubkey);
    index.try_into().expect("pubkey table index fits in u8")
}

fn kamino_usdc_reserve_instruction_constraint(
    discriminator: [u8; 8],
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_collateral_token_account: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> SquadsInstructionConstraint {
    kamino_reserve_instruction_constraint(
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_MAIN_MARKET,
        USDC_MINT,
        discriminator.to_vec(),
        vault,
        vault_usdc_token_account,
        vault_collateral_token_account,
        reserve_liquidity_supply,
    )
}

pub fn create_squads_program_interaction_kamino_usdc_reserve_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_collateral_token_account: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: vec![
                    kamino_usdc_reserve_instruction_constraint(
                        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
                        vault,
                        vault_usdc_token_account,
                        vault_collateral_token_account,
                        reserve_liquidity_supply,
                    ),
                    kamino_usdc_reserve_instruction_constraint(
                        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
                        vault,
                        vault_usdc_token_account,
                        vault_collateral_token_account,
                        reserve_liquidity_supply,
                    ),
                ],
                pre_hook: None,
                post_hook: None,
                spending_limits: vec![],
            },
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn create_squads_program_interaction_main_to_prime_usdc_route_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_main_usdc_collateral_token_account: Pubkey,
    main_usdc_reserve_liquidity_supply: Pubkey,
    vault_prime_usdc_collateral_token_account: Pubkey,
    prime_usdc_reserve_liquidity_supply: Pubkey,
    main_usdc_withdraw_data: Vec<u8>,
    prime_usdc_deposit_data: Vec<u8>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_reserve_instruction_constraint(
                KAMINO_MAIN_USDC_RESERVE,
                KAMINO_MAIN_MARKET,
                USDC_MINT,
                main_usdc_withdraw_data,
                vault,
                vault_usdc_token_account,
                vault_main_usdc_collateral_token_account,
                main_usdc_reserve_liquidity_supply,
            ),
            kamino_reserve_instruction_constraint(
                KAMINO_PRIME_USDC_RESERVE,
                KAMINO_PRIME_MARKET,
                USDC_MINT,
                prime_usdc_deposit_data,
                vault,
                vault_usdc_token_account,
                vault_prime_usdc_collateral_token_account,
                prime_usdc_reserve_liquidity_supply,
            ),
        ],
    )
}

pub fn create_squads_program_interaction_prime_usdc_to_pyusd_reserves_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    vault_pyusd_token_account: Pubkey,
    vault_prime_usdc_collateral_token_account: Pubkey,
    prime_usdc_reserve_liquidity_supply: Pubkey,
    vault_pyusd_collateral_token_account: Pubkey,
    pyusd_reserve_liquidity_supply: Pubkey,
    prime_usdc_withdraw_data: Vec<u8>,
    pyusd_deposit_data: Vec<u8>,
) -> Instruction {
    create_squads_program_interaction_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        vec![
            kamino_reserve_instruction_constraint(
                KAMINO_PRIME_USDC_RESERVE,
                KAMINO_PRIME_MARKET,
                USDC_MINT,
                prime_usdc_withdraw_data,
                vault,
                vault_usdc_token_account,
                vault_prime_usdc_collateral_token_account,
                prime_usdc_reserve_liquidity_supply,
            ),
            kamino_reserve_instruction_constraint(
                KAMINO_MAIN_PYUSD_RESERVE,
                KAMINO_MAIN_MARKET,
                PYUSD_MINT,
                pyusd_deposit_data,
                vault,
                vault_pyusd_token_account,
                vault_pyusd_collateral_token_account,
                pyusd_reserve_liquidity_supply,
            ),
        ],
    )
}

fn create_squads_program_interaction_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: constraints,
                pre_hook: None,
                post_hook: None,
                spending_limits: vec![],
            },
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn remove_squads_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
) -> Instruction {
    let action = SquadsSettingsAction::PolicyRemove { policy };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn execute_squads_spending_limit_withdrawal_instruction(
    policy: Pubkey,
    signer: Pubkey,
    squads_settings: Pubkey,
    source_account_index: u8,
    destination: Pubkey,
    lamports: u64,
) -> Instruction {
    let (vault, _) = derive_squads_vault(&squads_settings, source_account_index);
    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(policy, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(vault, false),
            AccountMeta::new(destination, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: serialize_squads_sync_policy_payload_args(
            source_account_index,
            SquadsPolicyPayload::SpendingLimit(SquadsSpendingLimitPayload {
                amount: lamports,
                destination,
                decimals: SOL_DECIMALS,
            }),
        ),
    }
}

pub fn execute_squads_program_interaction_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(policy, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_policy_payload_args(
            account_index,
            SquadsPolicyPayload::ProgramInteraction(SquadsProgramInteractionPayload {
                instruction_constraint_indices: Some(instruction_constraint_indices),
                transaction_payload: SquadsProgramInteractionTransactionPayload::SyncTransaction(
                    SquadsProgramInteractionSyncPayload {
                        account_index,
                        instructions: squads_compiled_instruction_payload(&compiled_instructions),
                    },
                ),
            }),
        ),
    }
}

pub fn hash_squads_account_metas(accounts: &[AccountMeta]) -> [u8; 32] {
    let mut bytes = Vec::with_capacity(accounts.len() * 34);
    for account in accounts {
        bytes.extend_from_slice(account.pubkey.as_ref());
        bytes.push(u8::from(account.is_writable));
        bytes.push(u8::from(account.is_signer));
    }

    hashv(&[&bytes]).to_bytes()
}

pub fn add_squads_program_from_env(svm: &mut LiteSVM) -> std::io::Result<Option<PathBuf>> {
    let Some(path) = env::var_os("SQUADS_SMART_ACCOUNT_PROGRAM_SO").map(PathBuf::from) else {
        return Ok(None);
    };
    let program = fs::read(&path)?;
    svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
        .map_err(|error| std::io::Error::other(format!("add Squads program failed: {error}")))?;
    Ok(Some(path))
}

pub fn add_squads_program_from_env_or_sibling_checkout(
    svm: &mut LiteSVM,
) -> std::io::Result<Option<PathBuf>> {
    if let Some(path) = add_squads_program_from_env(svm)? {
        return Ok(Some(path));
    }

    let fixture_path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .join(SQUADS_SMART_ACCOUNT_PROGRAM_SO_FIXTURE);
    if fixture_path.exists() {
        let program = fs::read(&fixture_path)?;
        svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
            .map_err(|error| {
                std::io::Error::other(format!("add Squads program failed: {error}"))
            })?;
        return Ok(Some(fixture_path));
    }

    let sibling_path =
        PathBuf::from("../passkey-work/target/deploy/squads_smart_account_program.so");
    if !sibling_path.exists() {
        return Ok(None);
    }

    let program = fs::read(&sibling_path)?;
    svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
        .map_err(|error| std::io::Error::other(format!("add Squads program failed: {error}")))?;
    Ok(Some(sibling_path))
}

pub fn create_funded_squads_test_context() -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config(FundedSquadsTestConfig::default())
}

pub fn create_funded_squads_test_context_with_mock_programs(
    mock_programs: &[MockProgram],
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config_and_mock_programs(
        FundedSquadsTestConfig::default(),
        mock_programs,
    )
}

pub fn create_funded_squads_test_context_with_config(
    config: FundedSquadsTestConfig,
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config_and_mock_programs(config, &[])
}

pub fn create_funded_squads_test_context_with_config_and_mock_programs(
    config: FundedSquadsTestConfig,
    mock_programs: &[MockProgram],
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    assert!(
        config.vault_funding_lamports < config.wallet_airdrop_lamports,
        "vault funding should leave the wallet funded for later operations"
    );

    let mut svm = new_litesvm();
    let Some(loaded_program_path) = add_squads_program_from_env_or_sibling_checkout(&mut svm)?
    else {
        return Ok(None);
    };
    for mock_program in mock_programs {
        match mock_program {
            MockProgram::Jupiter => {
                add_mock_jupiter_program(&mut svm)?;
            }
            MockProgram::KaminoLend => {
                add_mock_kamino_lend_program(&mut svm)?;
            }
            MockProgram::LoyalHubSwap => {
                add_loyal_hub_swap_program(&mut svm)?;
            }
        }
    }

    let wallet = Keypair::new();
    svm.airdrop(&wallet.pubkey(), config.wallet_airdrop_lamports)
        .expect("airdrop test wallet");
    seed_squads_program_config(&mut svm, wallet.pubkey(), squads_test_treasury(), 0);

    let pool = derive_squads_pool(config.smart_account_seed);
    let create_smart_account_ix = create_squads_smart_account_instruction(
        wallet.pubkey(),
        wallet.pubkey(),
        config.smart_account_seed,
    );
    send_instructions(&mut svm, &[create_smart_account_ix], &wallet);

    let settings_account = svm
        .get_account(&pool.settings)
        .expect("Squads settings account created");
    assert_eq!(settings_account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);

    let (vault, _) = derive_squads_vault(&pool.settings, config.vault_index);
    let fund_vault_ix =
        system_instruction::transfer(&wallet.pubkey(), &vault, config.vault_funding_lamports);
    send_instructions(&mut svm, &[fund_vault_ix], &wallet);

    let context = FundedSquadsTestContext {
        svm,
        wallet,
        pool,
        vault_index: config.vault_index,
        vault,
        wallet_airdrop_lamports: config.wallet_airdrop_lamports,
        vault_funding_lamports: config.vault_funding_lamports,
        loaded_program_path,
    };
    assert_eq!(context.vault_balance(), config.vault_funding_lamports);
    assert!(context.wallet_balance() > 0);

    Ok(Some(context))
}

pub fn send_instructions(svm: &mut LiteSVM, instructions: &[Instruction], payer: &Keypair) {
    try_send_instructions(svm, instructions, payer, &[]).unwrap();
}

pub fn try_send_instructions(
    svm: &mut LiteSVM,
    instructions: &[Instruction],
    payer: &Keypair,
    additional_signers: &[&Keypair],
) -> Result<(), String> {
    svm.expire_blockhash();
    let message =
        Message::new_with_blockhash(instructions, Some(&payer.pubkey()), &svm.latest_blockhash());
    let mut signers = Vec::with_capacity(additional_signers.len() + 1);
    signers.push(payer);
    signers.extend_from_slice(additional_signers);
    let transaction = Transaction::new(&signers, message, svm.latest_blockhash());
    svm.send_transaction(transaction)
        .map(|_| ())
        .map_err(|error| format!("{error:?}"))
}

pub fn hash32(value: &[u8]) -> [u8; 32] {
    hashv(&[value]).to_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn litesvm_can_fund_a_test_payer() {
        let mut svm = new_litesvm();
        let payer = Keypair::new();

        svm.airdrop(&payer.pubkey(), 1_000_000_000).unwrap();

        let account = svm.get_account(&payer.pubkey()).unwrap();
        assert_eq!(account.lamports, 1_000_000_000);
    }

    #[test]
    fn derives_squads_settings_and_vault_namespaces() {
        let pool = derive_squads_pool(1);
        let (settings_again, _) = derive_squads_settings(1);
        let (vault_0, _) = derive_squads_vault(&pool.settings, 0);
        let (vault_255, _) = derive_squads_vault(&pool.settings, u8::MAX);

        assert_eq!(pool.settings, settings_again);
        assert_ne!(vault_0, vault_255);
    }

    #[test]
    fn seeds_squads_program_config_in_litesvm() {
        let mut svm = new_litesvm();
        let authority = Pubkey::new_unique();
        let treasury = squads_test_treasury();

        let program_config = seed_squads_program_config(&mut svm, authority, treasury, 0);

        let account = svm.get_account(&program_config).unwrap();
        assert_eq!(account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(&account.data[..8], &SQUADS_PROGRAM_CONFIG_DISCRIMINATOR);
    }

    #[test]
    fn builds_squads_create_smart_account_instruction() {
        let payer = Pubkey::new_unique();
        let verifier = Pubkey::new_unique();
        let instruction = create_squads_smart_account_instruction(payer, verifier, 1);
        let (settings, _) = derive_squads_settings(1);

        assert_eq!(instruction.program_id, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(instruction.accounts[2], AccountMeta::new(payer, true));
        assert_eq!(instruction.accounts[5], AccountMeta::new(settings, false));
        assert_eq!(
            &instruction.data[..8],
            &SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR
        );
    }

    #[test]
    fn hashes_squads_payload_and_accounts_for_authorization() {
        let settings = derive_squads_pool(1).settings;
        let (vault, _) = derive_squads_vault(&settings, 0);
        let recipient = Pubkey::new_unique();
        let accounts = vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(recipient, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ];

        let payload = squads_system_transfer_payload(500_000);
        let accounts_hash = hash_squads_account_metas(&accounts);

        assert!(!payload.is_empty());
        assert_ne!(accounts_hash, [0; 32]);
    }
}
