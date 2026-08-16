//! Strict Jupiter Swap V2 `/build` validation for Loyal cross-mint routes.
//!
//! This module deliberately supports one narrow production contract: a one- or
//! two-hop AlphaQ ExactIn swap between canonical Earn stablecoins held by one
//! vault. It validates both classic SPL Token and Token-2022 account state,
//! while finalized credited deltas remain the economic source of truth.

use crate::stablecoins::EARN_STABLECOINS;
use crate::{
    derive_associated_token_account, earn_stablecoin, ASSOCIATED_TOKEN_PROGRAM_ID,
    JUPITER_SWAP_DISCRIMINATOR, JUPITER_V6_PROGRAM_ID, TOKEN_2022_PROGRAM_ID,
};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use serde::Deserialize;
use solana_sdk::{
    address_lookup_table::AddressLookupTableAccount,
    compute_budget::ComputeBudgetInstruction,
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0, VersionedMessage},
    program_pack::Pack,
    pubkey,
    pubkey::Pubkey,
    signature::Signature,
    transaction::VersionedTransaction,
};
use solana_system_interface::program as system_program;
use spl_token::state::{Account as SplTokenAccount, AccountState, Mint as SplMint};
use spl_token_2022::{
    extension::{
        default_account_state::DefaultAccountState,
        transfer_fee::{TransferFeeAmount, TransferFeeConfig},
        transfer_hook::{TransferHook, TransferHookAccount},
        BaseStateWithExtensions, ExtensionType, StateWithExtensions,
    },
    state::{
        Account as Token2022Account, AccountState as Token2022AccountState, Mint as Token2022Mint,
    },
};
use std::{
    collections::{BTreeMap, HashSet},
    str::FromStr,
};

pub const SOLANA_PACKET_DATA_SIZE: usize = 1_232;
pub const SOLANA_MAX_COMPUTE_UNITS: u32 = 1_400_000;
const COMPUTE_BUDGET_SET_UNIT_PRICE: u8 = 3;
const JUPITER_PLATFORM_FEE_BPS_OFFSET: usize = 26;
pub const JUPITER_ALPHAQ_ROUTE_OFFSET: usize = JUPITER_PLATFORM_FEE_BPS_OFFSET;
pub const JUPITER_ALPHAQ_ROUTE_BYTES: [u8; 14] = [0, 0, 0, 0, 1, 0, 0, 0, 104, 0, 16, 39, 0, 1];
pub const JUPITER_EVENT_AUTHORITY: Pubkey = pubkey!("D8cy77BBepLMngZx6ZukaTff5hCt1HrWyKk3Hnd9oitf");
pub const ALPHAQ_PROGRAM_ID: Pubkey = pubkey!("ALPHAQmeA7bjrVuccPsYPiCvsi428SNwte66Srvs4pHA");
const ALPHAQ_V2_ROUTE_VARIANT: u32 = 104;
const ALPHAQ_BASE_ACCOUNT_COUNT: usize = 14;
const ALPHAQ_TOKEN_2022_EXTRA_ACCOUNT_COUNT: usize = 2;
const MAX_ALPHAQ_ROUTE_STEPS: usize = 2;
const JUPITER_V2_FIRST_ROUTE_STEP_BYTES: usize = 9;
const JUPITER_V2_ADDITIONAL_ROUTE_STEP_BYTES: usize = 6;
const BPS_DENOMINATOR: u128 = 10_000;
pub const JUPITER_SHARED_ACCOUNTS_ROUTE_V2_DISCRIMINATOR: [u8; 8] =
    [209, 152, 83, 147, 124, 254, 216, 233];

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum JupiterV2Dialect {
    RouteV2,
    SharedAccountsRouteV2,
}

impl JupiterV2Dialect {
    pub const fn discriminator(self) -> [u8; 8] {
        match self {
            Self::RouteV2 => JUPITER_SWAP_DISCRIMINATOR,
            Self::SharedAccountsRouteV2 => JUPITER_SHARED_ACCOUNTS_ROUTE_V2_DISCRIMINATOR,
        }
    }

    pub const fn slippage_bps_offset(self) -> u64 {
        match self {
            Self::RouteV2 => 24,
            Self::SharedAccountsRouteV2 => 25,
        }
    }

    pub const fn platform_fee_bps_offset(self) -> u64 {
        match self {
            Self::RouteV2 => 26,
            Self::SharedAccountsRouteV2 => 27,
        }
    }

    pub(crate) const fn account_layout(self) -> JupiterV2AccountLayout {
        match self {
            Self::RouteV2 => JupiterV2AccountLayout {
                authority: 0,
                input_token_account: 1,
                output_token_account: 2,
                input_mint: 3,
                output_mint: 4,
                input_token_program: 5,
                output_token_program: 6,
                event_authority: 8,
                jupiter_program: 9,
            },
            Self::SharedAccountsRouteV2 => JupiterV2AccountLayout {
                authority: 1,
                input_token_account: 2,
                output_token_account: 5,
                input_mint: 6,
                output_mint: 7,
                input_token_program: 8,
                output_token_program: 9,
                event_authority: 10,
                jupiter_program: 11,
            },
        }
    }
}

/// Source side of one durable cross-mint policy.
///
/// The two shards are disjoint so a source mint has exactly one Squads
/// spending-limit state even though both Jupiter instruction dialects are
/// authorized. This keeps the configured daily cap aggregate across every
/// destination and dialect instead of accidentally duplicating it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum JupiterCrossMintSourceShard {
    Classic,
    Token2022,
}

impl JupiterCrossMintSourceShard {
    pub fn source_mints(self) -> [Pubkey; 3] {
        let token_program = self.source_token_program();
        let mut mints = [Pubkey::default(); 3];
        let mut count = 0;
        for stablecoin in EARN_STABLECOINS {
            if stablecoin.token_program == token_program {
                mints[count] = stablecoin.mint;
                count += 1;
            }
        }
        debug_assert_eq!(count, mints.len());
        mints
    }

    pub const fn source_token_program(self) -> Pubkey {
        match self {
            Self::Classic => spl_token::ID,
            Self::Token2022 => TOKEN_2022_PROGRAM_ID,
        }
    }

    pub fn contains(self, mint: Pubkey) -> bool {
        self.source_mints().contains(&mint)
    }
}

/// Durable cross-mint authority for one disjoint source-mint shard.
///
/// Every policy contains both supported Jupiter V2 ExactIn dialects and all
/// six canonical Earn destination ATAs. All V1 stablecoins have six decimals,
/// so one user-selected base-unit cap can be applied independently to each
/// source mint without a value-conversion oracle in the policy.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct JupiterCrossMintPolicySpec {
    pub source_shard: JupiterCrossMintSourceShard,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct JupiterCrossMintPolicySeeds {
    pub classic: u64,
    pub token_2022: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct JupiterV2AccountLayout {
    pub authority: u8,
    pub input_token_account: u8,
    pub output_token_account: u8,
    pub input_mint: u8,
    pub output_mint: u8,
    pub input_token_program: u8,
    pub output_token_program: u8,
    pub event_authority: u8,
    pub jupiter_program: u8,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterMintSnapshot {
    pub address: Pubkey,
    pub owner_program: Pubkey,
    pub data: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterTokenAccountSnapshot {
    pub address: Pubkey,
    pub owner_program: Pubkey,
    pub data: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterLookupTableSnapshot {
    pub address: Pubkey,
    pub addresses: Vec<Pubkey>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterExactInBuildExpectation {
    pub authority: Pubkey,
    pub input_mint: JupiterMintSnapshot,
    pub output_mint: JupiterMintSnapshot,
    pub input_token_account: JupiterTokenAccountSnapshot,
    pub output_token_account: JupiterTokenAccountSnapshot,
    /// Existing canonical vault ATAs that Jupiter may redundantly create for
    /// intermediate route mints. The worker omits setup only after these
    /// finalized snapshots prove the accounts already exist.
    pub additional_token_accounts: Vec<JupiterTokenAccountSnapshot>,
    pub input_amount: u64,
    pub minimum_output_amount: u64,
    pub maximum_slippage_bps: u16,
    pub requested_platform_fee_bps: u16,
    pub lookup_tables: Vec<JupiterLookupTableSnapshot>,
    pub limits: JupiterBuildLimits,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterBuildLimits {
    pub max_packet_bytes: usize,
    pub max_unique_accounts: usize,
    pub max_instructions: usize,
    pub max_lookup_tables: usize,
    pub max_lookup_addresses: usize,
    pub max_instruction_data_bytes: usize,
    pub max_compute_unit_price_micro_lamports: u64,
}

impl Default for JupiterBuildLimits {
    fn default() -> Self {
        Self {
            max_packet_bytes: SOLANA_PACKET_DATA_SIZE,
            max_unique_accounts: 64,
            max_instructions: 8,
            max_lookup_tables: 4,
            max_lookup_addresses: 4 * 256,
            max_instruction_data_bytes: 1_024,
            max_compute_unit_price_micro_lamports: 10_000_000,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterComputeBudget {
    pub unit_price_micro_lamports: u64,
    pub simulation_unit_limit: u32,
    pub maximum_unit_limit: u32,
}

impl JupiterComputeBudget {
    pub fn buffered_unit_limit(&self, simulated_units: u64) -> u32 {
        let buffered = simulated_units.saturating_mul(12).saturating_add(9) / 10;
        buffered.min(u64::from(self.maximum_unit_limit)) as u32
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterBuildStructure {
    pub instruction_count_with_compute_limit: usize,
    pub static_account_count: usize,
    pub lookup_account_count: usize,
    pub unique_account_count: usize,
    pub instruction_data_bytes: usize,
    pub packet_bytes_with_compute_limit: usize,
}

#[derive(Clone, Debug)]
pub struct ValidatedJupiterExactInBuild {
    pub dialect: JupiterV2Dialect,
    pub route_step_count: usize,
    pub input_mint: Pubkey,
    pub output_mint: Pubkey,
    pub input_amount: u64,
    pub quoted_output_amount: u64,
    pub minimum_output_amount: u64,
    pub slippage_bps: u16,
    pub compute_budget: JupiterComputeBudget,
    pub setup_instructions: Vec<Instruction>,
    pub swap_instruction: Instruction,
    pub lookup_tables: Vec<AddressLookupTableAccount>,
    pub recent_blockhash: Hash,
    pub last_valid_block_height: u64,
    pub blockhash_fetched_at: String,
    pub structure: JupiterBuildStructure,
}

impl ValidatedJupiterExactInBuild {
    pub fn instructions_with_compute_unit_limit(
        &self,
        compute_unit_limit: u32,
    ) -> Result<Vec<Instruction>, JupiterBuildError> {
        if compute_unit_limit == 0 || compute_unit_limit > self.compute_budget.maximum_unit_limit {
            return Err(JupiterBuildError::InvalidComputeUnitLimit(
                compute_unit_limit,
            ));
        }

        let mut instructions = Vec::with_capacity(self.setup_instructions.len() + 3);
        instructions.push(ComputeBudgetInstruction::set_compute_unit_limit(
            compute_unit_limit,
        ));
        instructions.push(ComputeBudgetInstruction::set_compute_unit_price(
            self.compute_budget.unit_price_micro_lamports,
        ));
        instructions.extend(self.setup_instructions.clone());
        instructions.push(self.swap_instruction.clone());
        Ok(instructions)
    }
}

#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum JupiterBuildError {
    #[error("invalid Jupiter /build JSON: {0}")]
    InvalidJson(String),
    #[error("invalid public key in {field}: {value}")]
    InvalidPubkey { field: &'static str, value: String },
    #[error("invalid base64 instruction data in {0}")]
    InvalidInstructionData(&'static str),
    #[error("mint or token account is not owned by its canonical token program")]
    InvalidTokenProgram,
    #[error("token account data is invalid")]
    InvalidTokenAccountData,
    #[error("mint account data is invalid or uninitialized")]
    InvalidMintAccountData,
    #[error("unsupported or active Token-2022 extension: {0}")]
    UnsupportedTokenExtension(&'static str),
    #[error("token account mint or authority does not match the expected vault binding")]
    InvalidTokenAccountBinding,
    #[error("input and output mints and token accounts must be distinct")]
    IdenticalSwapSides,
    #[error("the Jupiter pair is outside the canonical Earn stablecoin registry")]
    UnsupportedMintPair,
    #[error("Jupiter /build only accepts ExactIn")]
    InvalidSwapMode,
    #[error("Jupiter /build amount does not match the movement")]
    InvalidInputAmount,
    #[error("Jupiter /build minimum output threshold does not match the accepted quote")]
    InvalidMinimumOutput,
    #[error("Jupiter /build quoted output or slippage is invalid")]
    InvalidQuote,
    #[error("Jupiter platform fees are disabled")]
    PlatformFeeNotZero,
    #[error("Jupiter route must be one or two chained 100% AlphaQ steps")]
    InvalidRoutePlan,
    #[error("unsupported Jupiter V2 instruction dialect")]
    UnsupportedJupiterDialect,
    #[error("unexpected instruction program in {0}")]
    UnexpectedProgram(&'static str),
    #[error("unexpected signer or writable authority in {0}")]
    InvalidAuthority(&'static str),
    #[error("Jupiter swap core accounts do not match the accepted contract")]
    InvalidSwapAccounts,
    #[error("Jupiter AlphaQ residual accounts do not match the accepted contract")]
    InvalidResidualAccounts,
    #[error("Jupiter route metadata, encoded route, and AMM account do not agree")]
    RouteAmmMismatch,
    #[error("unsupported Jupiter setup instruction")]
    InvalidSetupInstruction,
    #[error("cleanup instructions are disabled")]
    CleanupInstructionNotAllowed,
    #[error("other instructions are disabled")]
    OtherInstructionsNotAllowed,
    #[error("tip instructions are disabled")]
    TipInstructionNotAllowed,
    #[error("invalid Jupiter ExactIn instruction data")]
    InvalidSwapInstructionData,
    #[error("Jupiter must return exactly one supported compute-unit-price instruction")]
    InvalidComputeBudget,
    #[error("compute-unit price exceeds the configured limit")]
    ComputeUnitPriceTooHigh,
    #[error("compute-unit limit must be within 1..={SOLANA_MAX_COMPUTE_UNITS}")]
    InvalidComputeUnitLimit(u32),
    #[error("Jupiter lookup tables are unresolved, inconsistent, or exceed configured bounds")]
    InvalidLookupTables,
    #[error("Jupiter blockhash metadata is missing or invalid")]
    InvalidBlockhashMetadata,
    #[error("Jupiter build exceeds an instruction or instruction-data limit")]
    InstructionLimitExceeded,
    #[error("Jupiter build exceeds the configured unique account limit")]
    AccountLimitExceeded,
    #[error("Jupiter build is {actual} bytes, above the {maximum}-byte packet limit")]
    PacketTooLarge { actual: usize, maximum: usize },
    #[error("could not compile the Jupiter v0 message: {0}")]
    MessageCompile(String),
}

pub fn parse_and_validate_jupiter_exact_in_build(
    json: &[u8],
    expected: &JupiterExactInBuildExpectation,
) -> Result<ValidatedJupiterExactInBuild, JupiterBuildError> {
    let build: RawJupiterBuild = serde_json::from_slice(json)
        .map_err(|error| JupiterBuildError::InvalidJson(error.to_string()))?;
    validate_expectation(expected)?;

    let input_mint = parse_pubkey("inputMint", &build.input_mint)?;
    let output_mint = parse_pubkey("outputMint", &build.output_mint)?;
    if input_mint != expected.input_mint.address || output_mint != expected.output_mint.address {
        return Err(JupiterBuildError::InvalidTokenAccountBinding);
    }
    if build.swap_mode != "ExactIn" {
        return Err(JupiterBuildError::InvalidSwapMode);
    }

    let input_amount = parse_amount(&build.in_amount)?;
    let quoted_output_amount = parse_amount(&build.out_amount)?;
    let minimum_output_amount = parse_amount(&build.other_amount_threshold)?;
    if input_amount == 0 || input_amount != expected.input_amount {
        return Err(JupiterBuildError::InvalidInputAmount);
    }
    if minimum_output_amount == 0 || minimum_output_amount != expected.minimum_output_amount {
        return Err(JupiterBuildError::InvalidMinimumOutput);
    }
    if build.slippage_bps > expected.maximum_slippage_bps
        || build.slippage_bps > BPS_DENOMINATOR as u16
        || quoted_output_amount < minimum_output_amount
        || threshold_for(quoted_output_amount, build.slippage_bps) != minimum_output_amount
    {
        return Err(JupiterBuildError::InvalidQuote);
    }
    if expected.requested_platform_fee_bps != 0 || build.platform_fee_bps.unwrap_or(0) != 0 {
        return Err(JupiterBuildError::PlatformFeeNotZero);
    }

    let route_steps = validate_route_plan(
        &build,
        input_mint,
        output_mint,
        input_amount,
        quoted_output_amount,
    )?;
    reject_extra_instructions(&build)?;

    let compute_budget = validate_compute_budget(&build, &expected.limits)?;
    let setup_instructions = build
        .setup_instructions
        .iter()
        .map(|instruction| validate_setup_instruction(instruction, expected))
        .collect::<Result<Vec<_>, _>>()?;
    if setup_instructions
        .iter()
        .map(|instruction| instruction.accounts[1].pubkey)
        .collect::<HashSet<_>>()
        .len()
        != setup_instructions.len()
    {
        return Err(JupiterBuildError::InvalidSetupInstruction);
    }
    let (swap_instruction, dialect) =
        validate_swap_instruction(&build.swap_instruction, &build, expected, &route_steps)?;
    let lookup_tables = validate_lookup_tables(&build, expected)?;
    let (recent_blockhash, last_valid_block_height, blockhash_fetched_at) =
        validate_blockhash(&build.blockhash_with_metadata)?;

    let mut compile_instructions = Vec::with_capacity(setup_instructions.len() + 3);
    compile_instructions.push(ComputeBudgetInstruction::set_compute_unit_limit(
        SOLANA_MAX_COMPUTE_UNITS,
    ));
    compile_instructions.push(ComputeBudgetInstruction::set_compute_unit_price(
        compute_budget.unit_price_micro_lamports,
    ));
    compile_instructions.extend(setup_instructions.clone());
    compile_instructions.push(swap_instruction.clone());

    let structure = compile_and_validate_structure(
        expected.authority,
        &compile_instructions,
        &lookup_tables,
        recent_blockhash,
        &expected.limits,
    )?;

    Ok(ValidatedJupiterExactInBuild {
        dialect,
        route_step_count: route_steps.len(),
        input_mint,
        output_mint,
        input_amount,
        quoted_output_amount,
        minimum_output_amount,
        slippage_bps: build.slippage_bps,
        compute_budget,
        setup_instructions,
        swap_instruction,
        lookup_tables,
        recent_blockhash,
        last_valid_block_height,
        blockhash_fetched_at,
        structure,
    })
}

fn validate_expectation(
    expected: &JupiterExactInBuildExpectation,
) -> Result<(), JupiterBuildError> {
    if expected.input_mint.address == expected.output_mint.address
        || expected.input_token_account.address == expected.output_token_account.address
    {
        return Err(JupiterBuildError::IdenticalSwapSides);
    }
    let input_asset = earn_stablecoin(expected.input_mint.address)
        .ok_or(JupiterBuildError::UnsupportedMintPair)?;
    let output_asset = earn_stablecoin(expected.output_mint.address)
        .ok_or(JupiterBuildError::UnsupportedMintPair)?;
    if input_asset.mint == output_asset.mint {
        return Err(JupiterBuildError::UnsupportedMintPair);
    }
    if expected.input_amount == 0 || expected.minimum_output_amount == 0 {
        return Err(JupiterBuildError::InvalidInputAmount);
    }
    if expected.maximum_slippage_bps > BPS_DENOMINATOR as u16 {
        return Err(JupiterBuildError::InvalidQuote);
    }
    validate_mint(
        &expected.input_mint,
        input_asset.token_program,
        input_asset.decimals,
    )?;
    validate_mint(
        &expected.output_mint,
        output_asset.token_program,
        output_asset.decimals,
    )?;
    validate_token_account(
        &expected.input_token_account,
        expected.input_mint.address,
        expected.authority,
        input_asset.token_program,
    )?;
    validate_token_account(
        &expected.output_token_account,
        expected.output_mint.address,
        expected.authority,
        output_asset.token_program,
    )?;
    if expected.input_token_account.address
        != derive_associated_token_account(
            expected.authority,
            input_asset.mint,
            input_asset.token_program,
        )
        || expected.output_token_account.address
            != derive_associated_token_account(
                expected.authority,
                output_asset.mint,
                output_asset.token_program,
            )
    {
        return Err(JupiterBuildError::InvalidTokenAccountBinding);
    }
    let mut seen_token_accounts = HashSet::from([
        expected.input_token_account.address,
        expected.output_token_account.address,
    ]);
    for snapshot in &expected.additional_token_accounts {
        if !seen_token_accounts.insert(snapshot.address) {
            return Err(JupiterBuildError::InvalidTokenAccountBinding);
        }
        let (mint, authority) = token_account_binding(snapshot)?;
        let asset = earn_stablecoin(mint).ok_or(JupiterBuildError::UnsupportedMintPair)?;
        if snapshot.owner_program != asset.token_program
            || authority != expected.authority
            || snapshot.address
                != derive_associated_token_account(
                    expected.authority,
                    asset.mint,
                    asset.token_program,
                )
        {
            return Err(JupiterBuildError::InvalidTokenAccountBinding);
        }
    }
    if expected.requested_platform_fee_bps != 0 {
        return Err(JupiterBuildError::PlatformFeeNotZero);
    }
    if expected.limits.max_packet_bytes == 0
        || expected.limits.max_packet_bytes > SOLANA_PACKET_DATA_SIZE
        || expected.limits.max_unique_accounts == 0
        || expected.limits.max_instructions == 0
        || expected.limits.max_lookup_tables == 0
        || expected.limits.max_lookup_addresses == 0
        || expected.limits.max_instruction_data_bytes == 0
    {
        return Err(JupiterBuildError::InstructionLimitExceeded);
    }
    Ok(())
}

fn validate_mint(
    mint: &JupiterMintSnapshot,
    token_program: Pubkey,
    expected_decimals: u8,
) -> Result<(), JupiterBuildError> {
    if mint.owner_program != token_program {
        return Err(JupiterBuildError::InvalidTokenProgram);
    }
    let (is_initialized, decimals) = if token_program == spl_token::id() {
        let mint =
            SplMint::unpack(&mint.data).map_err(|_| JupiterBuildError::InvalidMintAccountData)?;
        (mint.is_initialized, mint.decimals)
    } else if token_program == TOKEN_2022_PROGRAM_ID {
        let mint = StateWithExtensions::<Token2022Mint>::unpack(&mint.data)
            .map_err(|_| JupiterBuildError::InvalidMintAccountData)?;
        validate_token_2022_mint_extensions(&mint)?;
        (mint.base.is_initialized, mint.base.decimals)
    } else {
        return Err(JupiterBuildError::InvalidTokenProgram);
    };
    if !is_initialized || decimals != expected_decimals {
        return Err(JupiterBuildError::InvalidMintAccountData);
    }
    Ok(())
}

fn validate_token_account(
    snapshot: &JupiterTokenAccountSnapshot,
    expected_mint: Pubkey,
    expected_authority: Pubkey,
    token_program: Pubkey,
) -> Result<(), JupiterBuildError> {
    if snapshot.owner_program != token_program {
        return Err(JupiterBuildError::InvalidTokenProgram);
    }
    let (mint, owner) = token_account_binding(snapshot)?;
    if mint != expected_mint || owner != expected_authority {
        return Err(JupiterBuildError::InvalidTokenAccountBinding);
    }
    Ok(())
}

fn token_account_binding(
    snapshot: &JupiterTokenAccountSnapshot,
) -> Result<(Pubkey, Pubkey), JupiterBuildError> {
    let (initialized, mint, owner) = if snapshot.owner_program == spl_token::id() {
        let account = SplTokenAccount::unpack(&snapshot.data)
            .map_err(|_| JupiterBuildError::InvalidTokenAccountData)?;
        (
            account.state == AccountState::Initialized,
            account.mint,
            account.owner,
        )
    } else if snapshot.owner_program == TOKEN_2022_PROGRAM_ID {
        let account = StateWithExtensions::<Token2022Account>::unpack(&snapshot.data)
            .map_err(|_| JupiterBuildError::InvalidTokenAccountData)?;
        validate_token_2022_account_extensions(&account)?;
        (
            account.base.state == Token2022AccountState::Initialized,
            account.base.mint,
            account.base.owner,
        )
    } else {
        return Err(JupiterBuildError::InvalidTokenProgram);
    };
    if !initialized {
        return Err(JupiterBuildError::InvalidTokenAccountBinding);
    }
    Ok((mint, owner))
}

fn validate_token_2022_mint_extensions(
    mint: &StateWithExtensions<'_, Token2022Mint>,
) -> Result<(), JupiterBuildError> {
    for extension_type in mint
        .get_extension_types()
        .map_err(|_| JupiterBuildError::InvalidMintAccountData)?
    {
        match extension_type {
            ExtensionType::MintCloseAuthority
            | ExtensionType::ConfidentialTransferMint
            | ExtensionType::PermanentDelegate
            | ExtensionType::ConfidentialTransferFeeConfig
            | ExtensionType::MetadataPointer
            | ExtensionType::TokenMetadata => {}
            ExtensionType::DefaultAccountState => {
                let extension = mint
                    .get_extension::<DefaultAccountState>()
                    .map_err(|_| JupiterBuildError::InvalidMintAccountData)?;
                if extension.state != Token2022AccountState::Initialized as u8 {
                    return Err(JupiterBuildError::UnsupportedTokenExtension(
                        "default account state is not initialized",
                    ));
                }
            }
            ExtensionType::TransferFeeConfig => {
                let extension = mint
                    .get_extension::<TransferFeeConfig>()
                    .map_err(|_| JupiterBuildError::InvalidMintAccountData)?;
                if u16::from(extension.older_transfer_fee.transfer_fee_basis_points) != 0
                    || u64::from(extension.older_transfer_fee.maximum_fee) != 0
                    || u16::from(extension.newer_transfer_fee.transfer_fee_basis_points) != 0
                    || u64::from(extension.newer_transfer_fee.maximum_fee) != 0
                {
                    return Err(JupiterBuildError::UnsupportedTokenExtension(
                        "nonzero transfer fee",
                    ));
                }
            }
            ExtensionType::TransferHook => {
                let extension = mint
                    .get_extension::<TransferHook>()
                    .map_err(|_| JupiterBuildError::InvalidMintAccountData)?;
                if Option::<Pubkey>::from(extension.program_id).is_some() {
                    return Err(JupiterBuildError::UnsupportedTokenExtension(
                        "active transfer hook",
                    ));
                }
            }
            _ => {
                return Err(JupiterBuildError::UnsupportedTokenExtension(
                    "unsupported mint extension",
                ))
            }
        }
    }
    Ok(())
}

fn validate_token_2022_account_extensions(
    account: &StateWithExtensions<'_, Token2022Account>,
) -> Result<(), JupiterBuildError> {
    for extension_type in account
        .get_extension_types()
        .map_err(|_| JupiterBuildError::InvalidTokenAccountData)?
    {
        match extension_type {
            ExtensionType::ImmutableOwner => {}
            ExtensionType::TransferFeeAmount => {
                let extension = account
                    .get_extension::<TransferFeeAmount>()
                    .map_err(|_| JupiterBuildError::InvalidTokenAccountData)?;
                if u64::from(extension.withheld_amount) != 0 {
                    return Err(JupiterBuildError::UnsupportedTokenExtension(
                        "token account has withheld transfer fees",
                    ));
                }
            }
            ExtensionType::TransferHookAccount => {
                let extension = account
                    .get_extension::<TransferHookAccount>()
                    .map_err(|_| JupiterBuildError::InvalidTokenAccountData)?;
                if bool::from(extension.transferring) {
                    return Err(JupiterBuildError::UnsupportedTokenExtension(
                        "token account is inside a transfer hook",
                    ));
                }
            }
            _ => {
                return Err(JupiterBuildError::UnsupportedTokenExtension(
                    "unsupported token-account extension",
                ))
            }
        }
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct ValidatedAlphaQRouteStep {
    amm_key: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    bps: u16,
}

fn validate_route_plan(
    build: &RawJupiterBuild,
    input_mint: Pubkey,
    output_mint: Pubkey,
    input_amount: u64,
    quoted_output_amount: u64,
) -> Result<Vec<ValidatedAlphaQRouteStep>, JupiterBuildError> {
    if build.route_plan.is_empty() || build.route_plan.len() > MAX_ALPHAQ_ROUTE_STEPS {
        return Err(JupiterBuildError::InvalidRoutePlan);
    }
    let mut previous_output_mint = input_mint;
    let mut previous_output_amount = input_amount;
    let mut seen_mints = HashSet::from([input_mint]);
    let mut seen_amms = HashSet::new();
    let mut validated = Vec::with_capacity(build.route_plan.len());
    for route in &build.route_plan {
        let amm_key = parse_pubkey("routePlan.ammKey", &route.swap_info.amm_key)?;
        let route_input_mint = parse_pubkey("routePlan.inputMint", &route.swap_info.input_mint)?;
        let route_output_mint = parse_pubkey("routePlan.outputMint", &route.swap_info.output_mint)?;
        let route_input_amount = parse_amount(&route.swap_info.in_amount)?;
        let route_output_amount = parse_amount(&route.swap_info.out_amount)?;
        if route.bps != 10_000
            || route.percent != Some(100.0)
            || amm_key == Pubkey::default()
            || !seen_amms.insert(amm_key)
            || route.swap_info.label != "AlphaQ"
            || route_input_mint != previous_output_mint
            || route_input_amount != previous_output_amount
            || route_output_mint == route_input_mint
            || !seen_mints.insert(route_output_mint)
            || earn_stablecoin(route_input_mint).is_none()
            || earn_stablecoin(route_output_mint).is_none()
            || route_output_amount == 0
        {
            return Err(JupiterBuildError::InvalidRoutePlan);
        }
        validated.push(ValidatedAlphaQRouteStep {
            amm_key,
            input_mint: route_input_mint,
            output_mint: route_output_mint,
            bps: route.bps,
        });
        previous_output_mint = route_output_mint;
        previous_output_amount = route_output_amount;
    }
    if previous_output_mint != output_mint || previous_output_amount != quoted_output_amount {
        return Err(JupiterBuildError::InvalidRoutePlan);
    }
    Ok(validated)
}

fn reject_extra_instructions(build: &RawJupiterBuild) -> Result<(), JupiterBuildError> {
    if build.cleanup_instruction.is_some() {
        return Err(JupiterBuildError::CleanupInstructionNotAllowed);
    }
    if !build.other_instructions.is_empty() {
        return Err(JupiterBuildError::OtherInstructionsNotAllowed);
    }
    if build.tip_instruction.is_some() {
        return Err(JupiterBuildError::TipInstructionNotAllowed);
    }
    Ok(())
}

fn validate_compute_budget(
    build: &RawJupiterBuild,
    limits: &JupiterBuildLimits,
) -> Result<JupiterComputeBudget, JupiterBuildError> {
    if build.compute_budget_instructions.len() != 1 {
        return Err(JupiterBuildError::InvalidComputeBudget);
    }
    let raw = &build.compute_budget_instructions[0];
    if parse_pubkey("computeBudgetInstructions.programId", &raw.program_id)?
        != solana_sdk::compute_budget::id()
        || !raw.accounts.is_empty()
    {
        return Err(JupiterBuildError::InvalidComputeBudget);
    }
    let data = decode_instruction_data(raw, "computeBudgetInstructions")?;
    if data.len() != 9 || data[0] != COMPUTE_BUDGET_SET_UNIT_PRICE {
        return Err(JupiterBuildError::InvalidComputeBudget);
    }
    let unit_price_micro_lamports = read_u64(&data[1..9]);
    if unit_price_micro_lamports > limits.max_compute_unit_price_micro_lamports {
        return Err(JupiterBuildError::ComputeUnitPriceTooHigh);
    }
    Ok(JupiterComputeBudget {
        unit_price_micro_lamports,
        simulation_unit_limit: SOLANA_MAX_COMPUTE_UNITS,
        maximum_unit_limit: SOLANA_MAX_COMPUTE_UNITS,
    })
}

fn validate_setup_instruction(
    raw: &RawInstruction,
    expected: &JupiterExactInBuildExpectation,
) -> Result<Instruction, JupiterBuildError> {
    if parse_pubkey("setupInstructions.programId", &raw.program_id)? != ASSOCIATED_TOKEN_PROGRAM_ID
    {
        return Err(JupiterBuildError::UnexpectedProgram("setupInstructions"));
    }
    let instruction = parse_instruction(raw, "setupInstructions")?;
    if instruction.accounts.len() != 6 || instruction.data.as_slice() != [1] {
        return Err(JupiterBuildError::InvalidSetupInstruction);
    }
    let token_account = [
        &expected.input_token_account,
        &expected.output_token_account,
    ]
    .into_iter()
    .chain(expected.additional_token_accounts.iter())
    .find(|snapshot| snapshot.address == instruction.accounts[1].pubkey)
    .ok_or(JupiterBuildError::InvalidSetupInstruction)?;
    let (mint, authority) = token_account_binding(token_account)?;
    let token_program = earn_stablecoin(mint)
        .ok_or(JupiterBuildError::InvalidSetupInstruction)?
        .token_program;
    let expected_ata = derive_associated_token_account(expected.authority, mint, token_program);
    let expected_accounts = [
        AccountMeta::new(expected.authority, true),
        AccountMeta::new(expected_ata, false),
        AccountMeta::new_readonly(expected.authority, false),
        AccountMeta::new_readonly(mint, false),
        AccountMeta::new_readonly(system_program::ID, false),
        AccountMeta::new_readonly(token_program, false),
    ];
    if authority != expected.authority
        || token_account.owner_program != token_program
        || instruction.accounts.as_slice() != expected_accounts
    {
        return Err(JupiterBuildError::InvalidSetupInstruction);
    }
    Ok(instruction)
}

fn validate_swap_instruction(
    raw: &RawInstruction,
    build: &RawJupiterBuild,
    expected: &JupiterExactInBuildExpectation,
    route_steps: &[ValidatedAlphaQRouteStep],
) -> Result<(Instruction, JupiterV2Dialect), JupiterBuildError> {
    if parse_pubkey("swapInstruction.programId", &raw.program_id)? != JUPITER_V6_PROGRAM_ID {
        return Err(JupiterBuildError::UnexpectedProgram("swapInstruction"));
    }
    let instruction = parse_instruction(raw, "swapInstruction")?;
    let dialect = match instruction.data.get(..8) {
        Some(discriminator) if discriminator == JUPITER_SWAP_DISCRIMINATOR => {
            JupiterV2Dialect::RouteV2
        }
        Some(discriminator) if discriminator == JUPITER_SHARED_ACCOUNTS_ROUTE_V2_DISCRIMINATOR => {
            JupiterV2Dialect::SharedAccountsRouteV2
        }
        _ => return Err(JupiterBuildError::UnsupportedJupiterDialect),
    };
    let directions =
        validate_v2_route_data(&instruction.data, build, expected, route_steps, dialect)?;
    validate_alphaq_accounts(
        &instruction.accounts,
        expected,
        route_steps,
        &directions,
        dialect,
    )?;
    Ok((instruction, dialect))
}

fn validate_v2_route_data(
    data: &[u8],
    build: &RawJupiterBuild,
    expected: &JupiterExactInBuildExpectation,
    route_steps: &[ValidatedAlphaQRouteStep],
    dialect: JupiterV2Dialect,
) -> Result<Vec<bool>, JupiterBuildError> {
    let amount_offset = match dialect {
        JupiterV2Dialect::RouteV2 => 8,
        JupiterV2Dialect::SharedAccountsRouteV2 => 9,
    };
    let quoted_output = parse_amount(&build.out_amount)?;
    let route_offset = dialect.platform_fee_bps_offset() as usize + 1;
    let expected_len = route_offset
        .saturating_add(4)
        .saturating_add(
            usize::from(!route_steps.is_empty()).saturating_mul(JUPITER_V2_FIRST_ROUTE_STEP_BYTES),
        )
        .saturating_add(
            route_steps
                .len()
                .saturating_sub(1)
                .saturating_mul(JUPITER_V2_ADDITIONAL_ROUTE_STEP_BYTES),
        );
    if data.len() != expected_len
        || read_u64(&data[amount_offset..amount_offset + 8]) != expected.input_amount
        || read_u64(&data[amount_offset + 8..amount_offset + 16]) != quoted_output
        || read_u16(
            &data[dialect.slippage_bps_offset() as usize
                ..dialect.slippage_bps_offset() as usize + 2],
        ) != build.slippage_bps
    {
        return Err(JupiterBuildError::InvalidSwapInstructionData);
    }
    if data[dialect.platform_fee_bps_offset() as usize] != 0 {
        return Err(JupiterBuildError::PlatformFeeNotZero);
    }
    let encoded_count = u32::from_be_bytes(
        data[route_offset..route_offset + 4]
            .try_into()
            .expect("validated route count slice"),
    ) as usize;
    if encoded_count != route_steps.len() {
        return Err(JupiterBuildError::RouteAmmMismatch);
    }

    let mut directions = Vec::with_capacity(route_steps.len());
    let mut offset = route_offset + 4;
    for (index, route) in route_steps.iter().enumerate() {
        let (variant, direction_offset) = if index == 0 {
            (
                u32::from_be_bytes(
                    data[offset..offset + 4]
                        .try_into()
                        .expect("validated AlphaQ variant slice"),
                ),
                offset + 4,
            )
        } else {
            (u32::from(data[offset]), offset + 1)
        };
        let direction = data[direction_offset];
        let bps = read_u16(&data[direction_offset + 1..direction_offset + 3]);
        if variant != ALPHAQ_V2_ROUTE_VARIANT
            || direction > 1
            || bps != route.bps
            || data[direction_offset + 3] != index as u8
            || data[direction_offset + 4] != index as u8 + 1
        {
            return Err(JupiterBuildError::RouteAmmMismatch);
        }
        directions.push(direction == 1);
        offset += if index == 0 {
            JUPITER_V2_FIRST_ROUTE_STEP_BYTES
        } else {
            JUPITER_V2_ADDITIONAL_ROUTE_STEP_BYTES
        };
    }
    Ok(directions)
}

fn validate_alphaq_accounts(
    accounts: &[AccountMeta],
    expected: &JupiterExactInBuildExpectation,
    route_steps: &[ValidatedAlphaQRouteStep],
    directions: &[bool],
    dialect: JupiterV2Dialect,
) -> Result<(), JupiterBuildError> {
    let input_token_program = earn_stablecoin(expected.input_mint.address)
        .ok_or(JupiterBuildError::UnsupportedMintPair)?
        .token_program;
    let output_token_program = earn_stablecoin(expected.output_mint.address)
        .ok_or(JupiterBuildError::UnsupportedMintPair)?
        .token_program;
    if accounts.iter().enumerate().any(|(index, account)| {
        account.is_signer && index != dialect.account_layout().authority as usize
    }) {
        return Err(JupiterBuildError::InvalidAuthority("swapInstruction"));
    }
    let (core_len, shared_program_authority, first_route_account, final_route_account) =
        match dialect {
            JupiterV2Dialect::RouteV2 => {
                let core = [
                    AccountMeta::new_readonly(expected.authority, true),
                    AccountMeta::new(expected.input_token_account.address, false),
                    AccountMeta::new(expected.output_token_account.address, false),
                    AccountMeta::new_readonly(expected.input_mint.address, false),
                    AccountMeta::new_readonly(expected.output_mint.address, false),
                    AccountMeta::new_readonly(input_token_program, false),
                    AccountMeta::new_readonly(output_token_program, false),
                    AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
                    AccountMeta::new_readonly(JUPITER_EVENT_AUTHORITY, false),
                    AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
                ];
                if !accounts.starts_with(&core) {
                    return Err(JupiterBuildError::InvalidSwapAccounts);
                }
                (
                    core.len(),
                    None,
                    expected.input_token_account.address,
                    expected.output_token_account.address,
                )
            }
            JupiterV2Dialect::SharedAccountsRouteV2 => {
                if accounts.len() < 12 {
                    return Err(JupiterBuildError::InvalidSwapAccounts);
                }
                let program_authority = accounts[0].pubkey;
                let expected_fixed = [
                    (0, program_authority, false, false),
                    (1, expected.authority, true, false),
                    (2, expected.input_token_account.address, false, true),
                    (6, expected.input_mint.address, false, false),
                    (7, expected.output_mint.address, false, false),
                    (8, input_token_program, false, false),
                    (9, output_token_program, false, false),
                    (10, JUPITER_EVENT_AUTHORITY, false, false),
                    (11, JUPITER_V6_PROGRAM_ID, false, false),
                ];
                if program_authority == Pubkey::default()
                    || expected_fixed
                        .iter()
                        .any(|(index, pubkey, signer, writable)| {
                            !account_meta_is(&accounts[*index], *pubkey, *signer, *writable)
                        })
                    || [3usize, 4, 5].iter().any(|index| {
                        accounts[*index].pubkey == Pubkey::default()
                            || accounts[*index].is_signer
                            || !accounts[*index].is_writable
                    })
                    || accounts[5].pubkey != expected.output_token_account.address
                {
                    return Err(JupiterBuildError::InvalidSwapAccounts);
                }
                (
                    12,
                    Some(program_authority),
                    accounts[3].pubkey,
                    accounts[4].pubkey,
                )
            }
        };
    let expected_account_count = core_len
        + route_steps
            .iter()
            .map(alphaq_segment_account_count)
            .collect::<Result<Vec<_>, _>>()?
            .into_iter()
            .sum::<usize>();
    if accounts.len() != expected_account_count || directions.len() != route_steps.len() {
        return Err(JupiterBuildError::InvalidSwapAccounts);
    }

    let instructions_sysvar = solana_sdk::sysvar::instructions::id();
    let protected = [
        expected.authority,
        expected.input_token_account.address,
        expected.output_token_account.address,
        expected.input_mint.address,
        expected.output_mint.address,
        spl_token::id(),
        TOKEN_2022_PROGRAM_ID,
        system_program::ID,
        ASSOCIATED_TOKEN_PROGRAM_ID,
        solana_sdk::compute_budget::id(),
        JUPITER_V6_PROGRAM_ID,
        JUPITER_EVENT_AUTHORITY,
        ALPHAQ_PROGRAM_ID,
        instructions_sysvar,
    ];
    let mut cursor = core_len;
    let mut expected_source_account = first_route_account;
    for (index, (route, direction)) in route_steps.iter().zip(directions).enumerate() {
        let segment_len = alphaq_segment_account_count(route)?;
        let segment = &accounts[cursor..cursor + segment_len];
        let fixed = [
            (0, ALPHAQ_PROGRAM_ID, false, false),
            (2, route.amm_key, false, false),
            (11, spl_token::id(), false, false),
            (12, instructions_sysvar, false, false),
            (segment_len - 1, JUPITER_V6_PROGRAM_ID, false, false),
        ];
        if fixed.iter().any(|(offset, pubkey, signer, writable)| {
            !account_meta_is(&segment[*offset], *pubkey, *signer, *writable)
        }) {
            return Err(JupiterBuildError::RouteAmmMismatch);
        }
        let (source, destination) = if *direction {
            (&segment[4], &segment[5])
        } else {
            (&segment[5], &segment[4])
        };
        let expected_route_authority = shared_program_authority
            .filter(|_| source.pubkey != expected.input_token_account.address)
            .unwrap_or(expected.authority);
        let expected_destination = (index + 1 == route_steps.len()).then_some(final_route_account);
        if !account_meta_is(&segment[1], expected_route_authority, false, false)
            || source.pubkey != expected_source_account
            || source.is_signer
            || !source.is_writable
            || destination.pubkey == Pubkey::default()
            || destination.is_signer
            || !destination.is_writable
            || source.pubkey == destination.pubkey
            || expected_destination.is_some_and(|pubkey| destination.pubkey != pubkey)
        {
            return Err(JupiterBuildError::InvalidResidualAccounts);
        }
        expected_source_account = destination.pubkey;

        let state = &segment[3];
        let pool_input = &segment[6];
        let pool_output = &segment[7];
        if [state, pool_input, pool_output].iter().any(|account| {
            account.pubkey == Pubkey::default()
                || protected.contains(&account.pubkey)
                || account.is_signer
                || !account.is_writable
        }) || pool_input.pubkey == pool_output.pubkey
            || state.pubkey == pool_input.pubkey
            || state.pubkey == pool_output.pubkey
            || !account_meta_is(&segment[8], pool_input.pubkey, false, false)
            || !account_meta_is(&segment[9], pool_output.pubkey, false, false)
            || !account_meta_is(&segment[10], pool_output.pubkey, false, true)
        {
            return Err(JupiterBuildError::InvalidResidualAccounts);
        }

        if segment_len == ALPHAQ_BASE_ACCOUNT_COUNT + ALPHAQ_TOKEN_2022_EXTRA_ACCOUNT_COUNT {
            let token_2022_mint =
                alphaq_token_2022_mint(route)?.ok_or(JupiterBuildError::InvalidResidualAccounts)?;
            if !account_meta_is(&segment[13], token_2022_mint, false, false)
                || !account_meta_is(&segment[14], TOKEN_2022_PROGRAM_ID, false, false)
            {
                return Err(JupiterBuildError::InvalidResidualAccounts);
            }
        }
        cursor += segment_len;
    }
    Ok(())
}

fn alphaq_segment_account_count(
    route: &ValidatedAlphaQRouteStep,
) -> Result<usize, JupiterBuildError> {
    Ok(ALPHAQ_BASE_ACCOUNT_COUNT
        + alphaq_token_2022_mint(route)?
            .map(|_| ALPHAQ_TOKEN_2022_EXTRA_ACCOUNT_COUNT)
            .unwrap_or_default())
}

fn alphaq_token_2022_mint(
    route: &ValidatedAlphaQRouteStep,
) -> Result<Option<Pubkey>, JupiterBuildError> {
    let input_is_token_2022 = earn_stablecoin(route.input_mint)
        .ok_or(JupiterBuildError::InvalidRoutePlan)?
        .token_program
        == TOKEN_2022_PROGRAM_ID;
    let output_is_token_2022 = earn_stablecoin(route.output_mint)
        .ok_or(JupiterBuildError::InvalidRoutePlan)?
        .token_program
        == TOKEN_2022_PROGRAM_ID;
    match (input_is_token_2022, output_is_token_2022) {
        (true, false) => Ok(Some(route.input_mint)),
        (false, true) => Ok(Some(route.output_mint)),
        (false, false) => Ok(None),
        (true, true) => Err(JupiterBuildError::InvalidRoutePlan),
    }
}

fn account_meta_is(
    account: &AccountMeta,
    pubkey: Pubkey,
    is_signer: bool,
    is_writable: bool,
) -> bool {
    account.pubkey == pubkey && account.is_signer == is_signer && account.is_writable == is_writable
}

fn validate_lookup_tables(
    build: &RawJupiterBuild,
    expected: &JupiterExactInBuildExpectation,
) -> Result<Vec<AddressLookupTableAccount>, JupiterBuildError> {
    let limits = &expected.limits;
    if build.addresses_by_lookup_table_address.len() > limits.max_lookup_tables
        || build.addresses_by_lookup_table_address.len() != expected.lookup_tables.len()
    {
        return Err(JupiterBuildError::InvalidLookupTables);
    }
    let mut seen_tables = HashSet::new();
    let mut total = 0usize;
    let mut tables = Vec::with_capacity(build.addresses_by_lookup_table_address.len());
    for (table, addresses) in &build.addresses_by_lookup_table_address {
        if addresses.is_empty() || addresses.len() > 256 {
            return Err(JupiterBuildError::InvalidLookupTables);
        }
        total = total.saturating_add(addresses.len());
        if total > limits.max_lookup_addresses {
            return Err(JupiterBuildError::InvalidLookupTables);
        }
        let key = parse_pubkey("addressesByLookupTableAddress.key", table)?;
        if key == Pubkey::default() || !seen_tables.insert(key) {
            return Err(JupiterBuildError::InvalidLookupTables);
        }
        let mut parsed_addresses = Vec::with_capacity(addresses.len());
        for address in addresses {
            let address = parse_pubkey("addressesByLookupTableAddress.value", address)?;
            if address == Pubkey::default() {
                return Err(JupiterBuildError::InvalidLookupTables);
            }
            parsed_addresses.push(address);
        }
        let Some(snapshot) = expected
            .lookup_tables
            .iter()
            .find(|snapshot| snapshot.address == key)
        else {
            return Err(JupiterBuildError::InvalidLookupTables);
        };
        if snapshot.addresses != parsed_addresses {
            return Err(JupiterBuildError::InvalidLookupTables);
        }
        tables.push(AddressLookupTableAccount {
            key,
            addresses: parsed_addresses,
        });
    }
    Ok(tables)
}

fn validate_blockhash(
    metadata: &RawBlockhashMetadata,
) -> Result<(Hash, u64, String), JupiterBuildError> {
    let bytes: [u8; 32] = metadata
        .blockhash
        .as_slice()
        .try_into()
        .map_err(|_| JupiterBuildError::InvalidBlockhashMetadata)?;
    let fetched_at = metadata
        .fetched_at
        .canonical()
        .ok_or(JupiterBuildError::InvalidBlockhashMetadata)?;
    if bytes == [0; 32] || metadata.last_valid_block_height == 0 {
        return Err(JupiterBuildError::InvalidBlockhashMetadata);
    }
    Ok((
        Hash::new_from_array(bytes),
        metadata.last_valid_block_height,
        fetched_at,
    ))
}

fn compile_and_validate_structure(
    payer: Pubkey,
    instructions: &[Instruction],
    lookup_tables: &[AddressLookupTableAccount],
    recent_blockhash: Hash,
    limits: &JupiterBuildLimits,
) -> Result<JupiterBuildStructure, JupiterBuildError> {
    let instruction_data_bytes = instructions
        .iter()
        .map(|instruction| instruction.data.len())
        .sum::<usize>();
    if instructions.len() > limits.max_instructions
        || instruction_data_bytes > limits.max_instruction_data_bytes
    {
        return Err(JupiterBuildError::InstructionLimitExceeded);
    }
    let message = v0::Message::try_compile(&payer, instructions, lookup_tables, recent_blockhash)
        .map_err(|error| JupiterBuildError::MessageCompile(error.to_string()))?;
    let lookup_account_count = message
        .address_table_lookups
        .iter()
        .map(|lookup| lookup.writable_indexes.len() + lookup.readonly_indexes.len())
        .sum::<usize>();
    let unique_account_count = message.account_keys.len() + lookup_account_count;
    if unique_account_count > limits.max_unique_accounts {
        return Err(JupiterBuildError::AccountLimitExceeded);
    }
    let transaction = VersionedTransaction {
        signatures: vec![Signature::default(); usize::from(message.header.num_required_signatures)],
        message: VersionedMessage::V0(message.clone()),
    };
    let packet_bytes = bincode::serialized_size(&transaction)
        .map_err(|error| JupiterBuildError::MessageCompile(error.to_string()))?
        as usize;
    if packet_bytes > limits.max_packet_bytes {
        return Err(JupiterBuildError::PacketTooLarge {
            actual: packet_bytes,
            maximum: limits.max_packet_bytes,
        });
    }
    Ok(JupiterBuildStructure {
        instruction_count_with_compute_limit: instructions.len(),
        static_account_count: message.account_keys.len(),
        lookup_account_count,
        unique_account_count,
        instruction_data_bytes,
        packet_bytes_with_compute_limit: packet_bytes,
    })
}

fn parse_instruction(
    raw: &RawInstruction,
    field: &'static str,
) -> Result<Instruction, JupiterBuildError> {
    let program_id = parse_pubkey(field, &raw.program_id)?;
    let accounts = raw
        .accounts
        .iter()
        .map(|account| {
            let pubkey = parse_pubkey(field, &account.pubkey)?;
            Ok(AccountMeta {
                pubkey,
                is_signer: account.is_signer,
                is_writable: account.is_writable,
            })
        })
        .collect::<Result<Vec<_>, JupiterBuildError>>()?;
    let data = decode_instruction_data(raw, field)?;
    Ok(Instruction {
        program_id,
        accounts,
        data,
    })
}

fn decode_instruction_data(
    raw: &RawInstruction,
    field: &'static str,
) -> Result<Vec<u8>, JupiterBuildError> {
    BASE64_STANDARD
        .decode(&raw.data)
        .map_err(|_| JupiterBuildError::InvalidInstructionData(field))
}

fn parse_pubkey(field: &'static str, value: &str) -> Result<Pubkey, JupiterBuildError> {
    Pubkey::from_str(value).map_err(|_| JupiterBuildError::InvalidPubkey {
        field,
        value: value.to_owned(),
    })
}

fn parse_amount(value: &str) -> Result<u64, JupiterBuildError> {
    value.parse().map_err(|_| JupiterBuildError::InvalidQuote)
}

fn threshold_for(quoted_output: u64, slippage_bps: u16) -> u64 {
    let numerator = u128::from(quoted_output)
        .saturating_mul(BPS_DENOMINATOR.saturating_sub(u128::from(slippage_bps)));
    numerator
        .saturating_add(BPS_DENOMINATOR - 1)
        .checked_div(BPS_DENOMINATOR)
        .unwrap_or_default() as u64
}

fn read_u64(bytes: &[u8]) -> u64 {
    u64::from_le_bytes(bytes.try_into().expect("validated u64 slice"))
}

fn read_u16(bytes: &[u8]) -> u16 {
    u16::from_le_bytes(bytes.try_into().expect("validated u16 slice"))
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawJupiterBuild {
    input_mint: String,
    output_mint: String,
    in_amount: String,
    out_amount: String,
    other_amount_threshold: String,
    swap_mode: String,
    slippage_bps: u16,
    #[serde(default)]
    platform_fee_bps: Option<u16>,
    route_plan: Vec<RawRoutePlan>,
    compute_budget_instructions: Vec<RawInstruction>,
    setup_instructions: Vec<RawInstruction>,
    swap_instruction: RawInstruction,
    cleanup_instruction: Option<RawInstruction>,
    other_instructions: Vec<RawInstruction>,
    tip_instruction: Option<RawInstruction>,
    addresses_by_lookup_table_address: BTreeMap<String, Vec<String>>,
    blockhash_with_metadata: RawBlockhashMetadata,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawRoutePlan {
    swap_info: RawSwapInfo,
    #[serde(default)]
    percent: Option<f64>,
    bps: u16,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawSwapInfo {
    amm_key: String,
    label: String,
    input_mint: String,
    output_mint: String,
    in_amount: String,
    out_amount: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawInstruction {
    program_id: String,
    accounts: Vec<RawAccountMeta>,
    data: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawAccountMeta {
    pubkey: String,
    is_signer: bool,
    is_writable: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawBlockhashMetadata {
    blockhash: Vec<u8>,
    last_valid_block_height: u64,
    fetched_at: RawFetchedAt,
}

#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum RawFetchedAt {
    Text(String),
    Unix {
        secs_since_epoch: u64,
        nanos_since_epoch: u32,
    },
}

impl RawFetchedAt {
    fn canonical(&self) -> Option<String> {
        match self {
            Self::Text(value) if !value.trim().is_empty() => Some(value.clone()),
            Self::Unix {
                secs_since_epoch,
                nanos_since_epoch,
            } if *secs_since_epoch > 0 && *nanos_since_epoch < 1_000_000_000 => {
                Some(format!("{secs_since_epoch}.{nanos_since_epoch:09}Z"))
            }
            _ => None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{derive_classic_associated_token_account, USDC_MINT, USDT_MINT};
    use serde_json::{json, Value};
    use solana_sdk::program_option::COption;
    use spl_token::state::AccountState;
    use spl_token_2022::extension::{
        default_account_state::DefaultAccountState,
        immutable_owner::ImmutableOwner,
        non_transferable::NonTransferable,
        transfer_fee::{TransferFeeAmount, TransferFeeConfig},
        transfer_hook::{TransferHook, TransferHookAccount},
        BaseStateWithExtensionsMut, StateWithExtensionsMut,
    };

    const INPUT_AMOUNT: u64 = 1_000_000;
    const QUOTED_OUTPUT: u64 = 999_963;
    const MINIMUM_OUTPUT: u64 = 994_964;
    const SLIPPAGE_BPS: u16 = 50;

    struct Fixture {
        expected: JupiterExactInBuildExpectation,
        json: Value,
    }

    #[test]
    fn accepts_strict_classic_spl_exact_in_build_and_exposes_compilation_requirements() {
        let fixture = make_fixture();
        let validated = validate(&fixture).expect("strict classic SPL build");

        assert_eq!(validated.input_amount, INPUT_AMOUNT);
        assert_eq!(validated.quoted_output_amount, QUOTED_OUTPUT);
        assert_eq!(validated.minimum_output_amount, MINIMUM_OUTPUT);
        assert_eq!(validated.compute_budget.unit_price_micro_lamports, 345_323);
        assert_eq!(validated.lookup_tables.len(), 1);
        assert!(validated.structure.lookup_account_count > 0);
        assert!(validated.structure.packet_bytes_with_compute_limit <= SOLANA_PACKET_DATA_SIZE);
        assert_eq!(
            validated.compute_budget.buffered_unit_limit(200_001),
            240_002
        );
        assert_eq!(
            validated
                .instructions_with_compute_unit_limit(240_002)
                .unwrap()
                .len(),
            4
        );
        assert_eq!(
            validated
                .instructions_with_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS + 1)
                .unwrap_err(),
            JupiterBuildError::InvalidComputeUnitLimit(SOLANA_MAX_COMPUTE_UNITS + 1)
        );
    }

    #[test]
    fn accepts_only_inert_token_2022_transfer_extensions() {
        let mint_data = token_2022_mint_data(
            &[
                ExtensionType::DefaultAccountState,
                ExtensionType::TransferFeeConfig,
                ExtensionType::TransferHook,
            ],
            |mint| {
                mint.init_extension::<DefaultAccountState>(true)
                    .unwrap()
                    .state = Token2022AccountState::Initialized as u8;
                mint.init_extension::<TransferFeeConfig>(true).unwrap();
                mint.init_extension::<TransferHook>(true).unwrap();
            },
        );
        validate_mint(
            &JupiterMintSnapshot {
                address: Pubkey::new_unique(),
                owner_program: TOKEN_2022_PROGRAM_ID,
                data: mint_data,
            },
            TOKEN_2022_PROGRAM_ID,
            6,
        )
        .expect("disabled fees and hook are inert");

        let authority = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        let account_data = token_2022_account_data(
            mint,
            authority,
            &[
                ExtensionType::ImmutableOwner,
                ExtensionType::TransferFeeAmount,
                ExtensionType::TransferHookAccount,
            ],
            |account| {
                account.init_extension::<ImmutableOwner>(true).unwrap();
                account.init_extension::<TransferFeeAmount>(true).unwrap();
                account.init_extension::<TransferHookAccount>(true).unwrap();
            },
        );
        assert_eq!(
            token_account_binding(&JupiterTokenAccountSnapshot {
                address: Pubkey::new_unique(),
                owner_program: TOKEN_2022_PROGRAM_ID,
                data: account_data,
            }),
            Ok((mint, authority))
        );
    }

    #[test]
    fn rejects_active_token_2022_mint_transfer_controls() {
        let active_hook = token_2022_mint_data(&[ExtensionType::TransferHook], |mint| {
            mint.init_extension::<TransferHook>(true)
                .unwrap()
                .program_id = Some(Pubkey::new_unique()).try_into().unwrap();
        });
        assert_token_2022_mint_error(
            active_hook,
            JupiterBuildError::UnsupportedTokenExtension("active transfer hook"),
        );

        let nonzero_fee = token_2022_mint_data(&[ExtensionType::TransferFeeConfig], |mint| {
            let fee = mint.init_extension::<TransferFeeConfig>(true).unwrap();
            fee.newer_transfer_fee.transfer_fee_basis_points = 1u16.into();
            fee.newer_transfer_fee.maximum_fee = 1u64.into();
        });
        assert_token_2022_mint_error(
            nonzero_fee,
            JupiterBuildError::UnsupportedTokenExtension("nonzero transfer fee"),
        );

        let frozen_default = token_2022_mint_data(&[ExtensionType::DefaultAccountState], |mint| {
            mint.init_extension::<DefaultAccountState>(true)
                .unwrap()
                .state = Token2022AccountState::Frozen as u8;
        });
        assert_token_2022_mint_error(
            frozen_default,
            JupiterBuildError::UnsupportedTokenExtension(
                "default account state is not initialized",
            ),
        );
    }

    #[test]
    fn rejects_unmodeled_token_2022_extensions() {
        let nontransferable = token_2022_mint_data(&[ExtensionType::NonTransferable], |mint| {
            mint.init_extension::<NonTransferable>(true).unwrap();
        });
        assert_token_2022_mint_error(
            nontransferable,
            JupiterBuildError::UnsupportedTokenExtension("unsupported mint extension"),
        );

        let mint = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let withheld = token_2022_account_data(
            mint,
            authority,
            &[ExtensionType::TransferFeeAmount],
            |account| {
                account
                    .init_extension::<TransferFeeAmount>(true)
                    .unwrap()
                    .withheld_amount = 1u64.into();
            },
        );
        assert_eq!(
            token_account_binding(&JupiterTokenAccountSnapshot {
                address: Pubkey::new_unique(),
                owner_program: TOKEN_2022_PROGRAM_ID,
                data: withheld,
            }),
            Err(JupiterBuildError::UnsupportedTokenExtension(
                "token account has withheld transfer fees"
            ))
        );

        let transferring = token_2022_account_data(
            mint,
            authority,
            &[ExtensionType::TransferHookAccount],
            |account| {
                account
                    .init_extension::<TransferHookAccount>(true)
                    .unwrap()
                    .transferring = true.into();
            },
        );
        assert_eq!(
            token_account_binding(&JupiterTokenAccountSnapshot {
                address: Pubkey::new_unique(),
                owner_program: TOKEN_2022_PROGRAM_ID,
                data: transferring,
            }),
            Err(JupiterBuildError::UnsupportedTokenExtension(
                "token account is inside a transfer hook"
            ))
        );
    }

    #[test]
    fn rejects_amount_threshold_fee_route_and_token_program_mutations() {
        let mut fixture = make_fixture();
        fixture.expected.output_mint.address = Pubkey::new_unique();
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::UnsupportedMintPair
        );

        let mut fixture = make_fixture();
        fixture.json["inAmount"] = json!("999999");
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidInputAmount
        );

        let mut fixture = make_fixture();
        fixture.json["otherAmountThreshold"] = json!("994963");
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidMinimumOutput
        );

        let mut fixture = make_fixture();
        fixture.json["platformFeeBps"] = json!(1);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::PlatformFeeNotZero
        );

        let mut fixture = make_fixture();
        set_swap_instruction_u64(&mut fixture.json, 8, INPUT_AMOUNT - 1);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidSwapInstructionData
        );

        let mut fixture = make_fixture();
        set_swap_instruction_byte(&mut fixture.json, JUPITER_PLATFORM_FEE_BPS_OFFSET, 1);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::PlatformFeeNotZero
        );

        let mut fixture = make_fixture();
        fixture.json["routePlan"][0]["bps"] = json!(9_999);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidRoutePlan
        );

        let mut fixture = make_fixture();
        fixture.expected.output_mint.owner_program = TOKEN_2022_PROGRAM_ID;
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidTokenProgram
        );

        let mut fixture = make_fixture();
        fixture.expected.input_token_account.owner_program = TOKEN_2022_PROGRAM_ID;
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidTokenProgram
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][5]["pubkey"] =
            json!(TOKEN_2022_PROGRAM_ID.to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidSwapAccounts
        );
    }

    #[test]
    fn rejects_untrusted_instruction_authority_owner_and_extra_instruction_shapes() {
        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["programId"] = json!(Pubkey::new_unique().to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::UnexpectedProgram("swapInstruction")
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][0]["isWritable"] = json!(true);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidSwapAccounts
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][6]["isSigner"] = json!(true);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidAuthority("swapInstruction")
        );

        let mut fixture = make_fixture();
        fixture.expected.output_token_account.data =
            token_account_data(fixture.expected.output_mint.address, Pubkey::new_unique());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidTokenAccountBinding
        );

        let mut fixture = make_fixture();
        fixture.json["setupInstructions"][0]["data"] = json!(BASE64_STANDARD.encode([0]));
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidSetupInstruction
        );

        let mut fixture = make_fixture();
        fixture.json["setupInstructions"][0]["programId"] = json!(Pubkey::new_unique().to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::UnexpectedProgram("setupInstructions")
        );

        for (field, expected_error) in [
            (
                "cleanupInstruction",
                JupiterBuildError::CleanupInstructionNotAllowed,
            ),
            (
                "tipInstruction",
                JupiterBuildError::TipInstructionNotAllowed,
            ),
        ] {
            let mut fixture = make_fixture();
            fixture.json[field] = fixture.json["swapInstruction"].clone();
            assert_eq!(validate(&fixture).unwrap_err(), expected_error);
        }

        let mut fixture = make_fixture();
        fixture.json["otherInstructions"] = json!([fixture.json["swapInstruction"].clone()]);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::OtherInstructionsNotAllowed
        );
    }

    #[test]
    fn rejects_compute_alt_blockhash_account_and_packet_limit_violations() {
        let mut fixture = make_fixture();
        fixture.json["computeBudgetInstructions"][0]["data"] =
            json!(BASE64_STANDARD.encode([2, 0, 0, 0, 0]));
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidComputeBudget
        );

        let mut fixture = make_fixture();
        fixture
            .expected
            .limits
            .max_compute_unit_price_micro_lamports = 1;
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::ComputeUnitPriceTooHigh
        );

        let mut fixture = make_fixture();
        fixture.json["addressesByLookupTableAddress"] = json!({});
        fixture.expected.lookup_tables.clear();
        let validated = validate(&fixture).expect("bounded static-only Jupiter build");
        assert!(validated.lookup_tables.is_empty());
        assert_eq!(validated.structure.lookup_account_count, 0);

        let mut fixture = make_fixture();
        let unused = Pubkey::new_unique().to_string();
        let table = fixture.json["addressesByLookupTableAddress"]
            .as_object_mut()
            .unwrap();
        *table.values_mut().next().unwrap() = json!([unused]);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidLookupTables
        );

        let mut fixture = make_fixture();
        fixture.json["blockhashWithMetadata"]["blockhash"] = json!([1, 2, 3]);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidBlockhashMetadata
        );

        let mut fixture = make_fixture();
        fixture.expected.limits.max_unique_accounts = 5;
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::AccountLimitExceeded
        );

        let mut fixture = make_fixture();
        fixture.expected.limits.max_packet_bytes = 200;
        assert!(matches!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::PacketTooLarge { .. }
        ));
    }

    #[test]
    fn accepts_current_structured_blockhash_timestamp_and_rejects_invalid_bounds() {
        let mut fixture = make_fixture();
        fixture.json["blockhashWithMetadata"]["fetchedAt"] = json!({
            "secs_since_epoch": 1_786_835_417u64,
            "nanos_since_epoch": 728_332_394u32,
        });
        let validated = validate(&fixture).expect("structured Jupiter fetchedAt timestamp");
        assert_eq!(validated.blockhash_fetched_at, "1786835417.728332394Z");

        for fetched_at in [
            json!({"secs_since_epoch": 0, "nanos_since_epoch": 1}),
            json!({"secs_since_epoch": 1, "nanos_since_epoch": 1_000_000_000u64}),
            json!(""),
        ] {
            let mut fixture = make_fixture();
            fixture.json["blockhashWithMetadata"]["fetchedAt"] = fetched_at;
            assert_eq!(
                validate(&fixture).unwrap_err(),
                JupiterBuildError::InvalidBlockhashMetadata
            );
        }
    }

    #[test]
    fn accepts_finalized_lookup_tables_with_overlapping_addresses() {
        let mut fixture = make_fixture();
        let shared_address = fixture.expected.lookup_tables[0].addresses[0];
        let second_table = Pubkey::new_unique();
        fixture.json["addressesByLookupTableAddress"][second_table.to_string()] =
            json!([shared_address.to_string()]);
        fixture
            .expected
            .lookup_tables
            .push(JupiterLookupTableSnapshot {
                address: second_table,
                addresses: vec![shared_address],
            });

        let validated = validate(&fixture).expect("overlapping finalized lookup tables");
        assert_eq!(validated.lookup_tables.len(), 2);
    }

    #[test]
    fn rejects_unpositioned_token_2022_capability_account_for_classic_pair() {
        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"]
            .as_array_mut()
            .unwrap()
            .push(json!({
                "pubkey": TOKEN_2022_PROGRAM_ID.to_string(),
                "isSigner": false,
                "isWritable": false,
            }));

        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidSwapAccounts
        );
    }

    #[test]
    fn rejects_alphaq_route_amm_program_and_residual_account_mutations() {
        let mut fixture = make_fixture();
        fixture.json["routePlan"][0]["swapInfo"]["ammKey"] =
            json!(Pubkey::new_unique().to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::RouteAmmMismatch
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][10]["pubkey"] =
            json!(Pubkey::new_unique().to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::RouteAmmMismatch
        );

        let mut fixture = make_fixture();
        set_swap_instruction_byte(&mut fixture.json, JUPITER_ALPHAQ_ROUTE_OFFSET + 8, 103);
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::RouteAmmMismatch
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][16]["pubkey"] =
            json!(fixture.expected.input_token_account.address.to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidResidualAccounts
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][13]["pubkey"] =
            json!(system_program::ID.to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::InvalidResidualAccounts
        );

        let mut fixture = make_fixture();
        fixture.json["swapInstruction"]["accounts"][23]["pubkey"] =
            json!(Pubkey::new_unique().to_string());
        assert_eq!(
            validate(&fixture).unwrap_err(),
            JupiterBuildError::RouteAmmMismatch
        );
    }

    fn validate(fixture: &Fixture) -> Result<ValidatedJupiterExactInBuild, JupiterBuildError> {
        parse_and_validate_jupiter_exact_in_build(
            &serde_json::to_vec(&fixture.json).unwrap(),
            &fixture.expected,
        )
    }

    fn make_fixture() -> Fixture {
        let authority = Pubkey::new_unique();
        let input_mint = USDC_MINT;
        let output_mint = USDT_MINT;
        let input_token = derive_classic_associated_token_account(authority, input_mint);
        let output_token = derive_classic_associated_token_account(authority, output_mint);
        let route_account = Pubkey::new_unique();
        let alphaq_state = Pubkey::new_unique();
        let pool_input = Pubkey::new_unique();
        let pool_output = Pubkey::new_unique();
        let lookup_table = Pubkey::new_unique();
        let compute_price = [
            vec![COMPUTE_BUDGET_SET_UNIT_PRICE],
            345_323u64.to_le_bytes().to_vec(),
        ]
        .concat();
        let swap_data = [
            JUPITER_SWAP_DISCRIMINATOR.to_vec(),
            INPUT_AMOUNT.to_le_bytes().to_vec(),
            QUOTED_OUTPUT.to_le_bytes().to_vec(),
            SLIPPAGE_BPS.to_le_bytes().to_vec(),
            JUPITER_ALPHAQ_ROUTE_BYTES.to_vec(),
        ]
        .concat();

        let meta = |pubkey: Pubkey, is_signer: bool, is_writable: bool| {
            json!({
                "pubkey": pubkey.to_string(),
                "isSigner": is_signer,
                "isWritable": is_writable,
            })
        };
        let swap_instruction = json!({
            "programId": JUPITER_V6_PROGRAM_ID.to_string(),
            "accounts": [
                meta(authority, true, false),
                meta(input_token, false, true),
                meta(output_token, false, true),
                meta(input_mint, false, false),
                meta(output_mint, false, false),
                meta(spl_token::id(), false, false),
                meta(spl_token::id(), false, false),
                meta(JUPITER_V6_PROGRAM_ID, false, false),
                meta(JUPITER_EVENT_AUTHORITY, false, false),
                meta(JUPITER_V6_PROGRAM_ID, false, false),
                meta(ALPHAQ_PROGRAM_ID, false, false),
                meta(authority, false, false),
                meta(route_account, false, false),
                meta(alphaq_state, false, true),
                meta(output_token, false, true),
                meta(input_token, false, true),
                meta(pool_input, false, true),
                meta(pool_output, false, true),
                meta(pool_input, false, false),
                meta(pool_output, false, false),
                meta(pool_output, false, true),
                meta(spl_token::id(), false, false),
                meta(solana_sdk::sysvar::instructions::id(), false, false),
                meta(JUPITER_V6_PROGRAM_ID, false, false),
            ],
            "data": BASE64_STANDARD.encode(swap_data),
        });
        let json = json!({
            "inputMint": input_mint.to_string(),
            "outputMint": output_mint.to_string(),
            "inAmount": INPUT_AMOUNT.to_string(),
            "outAmount": QUOTED_OUTPUT.to_string(),
            "otherAmountThreshold": MINIMUM_OUTPUT.to_string(),
            "swapMode": "ExactIn",
            "slippageBps": SLIPPAGE_BPS,
            "platformFeeBps": 0,
            "routePlan": [{
                "percent": 100,
                "bps": 10_000,
                "swapInfo": {
                    "ammKey": route_account.to_string(),
                    "label": "AlphaQ",
                    "inputMint": input_mint.to_string(),
                    "outputMint": output_mint.to_string(),
                    "inAmount": INPUT_AMOUNT.to_string(),
                    "outAmount": QUOTED_OUTPUT.to_string(),
                }
            }],
            "computeBudgetInstructions": [{
                "programId": solana_sdk::compute_budget::id().to_string(),
                "accounts": [],
                "data": BASE64_STANDARD.encode(compute_price),
            }],
            "setupInstructions": [{
                "programId": ASSOCIATED_TOKEN_PROGRAM_ID.to_string(),
                "accounts": [
                    meta(authority, true, true),
                    meta(output_token, false, true),
                    meta(authority, false, false),
                    meta(output_mint, false, false),
                    meta(system_program::ID, false, false),
                    meta(spl_token::id(), false, false),
                ],
                "data": BASE64_STANDARD.encode([1]),
            }],
            "swapInstruction": swap_instruction,
            "cleanupInstruction": null,
            "otherInstructions": [],
            "tipInstruction": null,
            "addressesByLookupTableAddress": {
                lookup_table.to_string(): [route_account.to_string()]
            },
            "blockhashWithMetadata": {
                "blockhash": vec![7u8; 32],
                "lastValidBlockHeight": 123_456,
                "fetchedAt": "2026-08-15T00:00:00Z",
            }
        });
        Fixture {
            expected: JupiterExactInBuildExpectation {
                authority,
                input_mint: JupiterMintSnapshot {
                    address: input_mint,
                    owner_program: spl_token::id(),
                    data: mint_account_data(),
                },
                output_mint: JupiterMintSnapshot {
                    address: output_mint,
                    owner_program: spl_token::id(),
                    data: mint_account_data(),
                },
                input_token_account: JupiterTokenAccountSnapshot {
                    address: input_token,
                    owner_program: spl_token::id(),
                    data: token_account_data(input_mint, authority),
                },
                output_token_account: JupiterTokenAccountSnapshot {
                    address: output_token,
                    owner_program: spl_token::id(),
                    data: token_account_data(output_mint, authority),
                },
                additional_token_accounts: vec![],
                input_amount: INPUT_AMOUNT,
                minimum_output_amount: MINIMUM_OUTPUT,
                maximum_slippage_bps: SLIPPAGE_BPS,
                requested_platform_fee_bps: 0,
                lookup_tables: vec![JupiterLookupTableSnapshot {
                    address: lookup_table,
                    addresses: vec![route_account],
                }],
                limits: JupiterBuildLimits::default(),
            },
            json,
        }
    }

    fn token_account_data(mint: Pubkey, owner: Pubkey) -> Vec<u8> {
        let account = SplTokenAccount {
            mint,
            owner,
            amount: 1_000_000,
            delegate: COption::None,
            state: AccountState::Initialized,
            is_native: COption::None,
            delegated_amount: 0,
            close_authority: COption::None,
        };
        let mut data = vec![0; SplTokenAccount::LEN];
        SplTokenAccount::pack(account, &mut data).unwrap();
        data
    }

    fn mint_account_data() -> Vec<u8> {
        let mint = SplMint {
            mint_authority: COption::None,
            supply: 1_000_000_000,
            decimals: 6,
            is_initialized: true,
            freeze_authority: COption::None,
        };
        let mut data = vec![0; SplMint::LEN];
        SplMint::pack(mint, &mut data).unwrap();
        data
    }

    fn token_2022_mint_data(
        extension_types: &[ExtensionType],
        configure: impl FnOnce(&mut StateWithExtensionsMut<'_, Token2022Mint>),
    ) -> Vec<u8> {
        let account_len =
            ExtensionType::try_calculate_account_len::<Token2022Mint>(extension_types).unwrap();
        let mut data = vec![0; account_len];
        let mut mint = StateWithExtensionsMut::<Token2022Mint>::unpack_uninitialized(&mut data)
            .expect("uninitialized Token-2022 mint");
        configure(&mut mint);
        mint.base = Token2022Mint {
            mint_authority: COption::None,
            supply: 1_000_000_000,
            decimals: 6,
            is_initialized: true,
            freeze_authority: COption::None,
        };
        mint.pack_base();
        mint.init_account_type().unwrap();
        data
    }

    fn assert_token_2022_mint_error(data: Vec<u8>, expected: JupiterBuildError) {
        assert_eq!(
            validate_mint(
                &JupiterMintSnapshot {
                    address: Pubkey::new_unique(),
                    owner_program: TOKEN_2022_PROGRAM_ID,
                    data,
                },
                TOKEN_2022_PROGRAM_ID,
                6,
            ),
            Err(expected)
        );
    }

    fn token_2022_account_data(
        mint: Pubkey,
        owner: Pubkey,
        extension_types: &[ExtensionType],
        configure: impl FnOnce(&mut StateWithExtensionsMut<'_, Token2022Account>),
    ) -> Vec<u8> {
        let account_len =
            ExtensionType::try_calculate_account_len::<Token2022Account>(extension_types).unwrap();
        let mut data = vec![0; account_len];
        let mut account =
            StateWithExtensionsMut::<Token2022Account>::unpack_uninitialized(&mut data)
                .expect("uninitialized Token-2022 account");
        configure(&mut account);
        account.base = Token2022Account {
            mint,
            owner,
            amount: 1_000_000,
            delegate: COption::None,
            state: Token2022AccountState::Initialized,
            is_native: COption::None,
            delegated_amount: 0,
            close_authority: COption::None,
        };
        account.pack_base();
        account.init_account_type().unwrap();
        data
    }

    fn set_swap_instruction_u64(json: &mut Value, offset: usize, value: u64) {
        let mut data = BASE64_STANDARD
            .decode(json["swapInstruction"]["data"].as_str().unwrap())
            .unwrap();
        data[offset..offset + 8].copy_from_slice(&value.to_le_bytes());
        json["swapInstruction"]["data"] = json!(BASE64_STANDARD.encode(data));
    }

    fn set_swap_instruction_byte(json: &mut Value, offset: usize, value: u8) {
        let mut data = BASE64_STANDARD
            .decode(json["swapInstruction"]["data"].as_str().unwrap())
            .unwrap();
        data[offset] = value;
        json["swapInstruction"]["data"] = json!(BASE64_STANDARD.encode(data));
    }
}
