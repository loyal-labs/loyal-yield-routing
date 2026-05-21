#![allow(dead_code, unused_imports)]

use borsh::BorshSerialize;
use litesvm::LiteSVM;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
};
use std::{io::Write, path::PathBuf};

use crate::{
    execute_squads_sync_transfer_instruction, send_instructions, DEFAULT_WALLET_AIRDROP_LAMPORTS,
    YIELD_ROUTE_DEPOSIT_POLICY_SEED, YIELD_ROUTE_SWAP_POLICY_SEED,
    YIELD_ROUTE_WITHDRAW_POLICY_SEED,
};

#[derive(BorshSerialize)]
#[allow(dead_code)]
pub(crate) enum SquadsSettingsAction {
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
pub(crate) enum LegacyPeriod {
    OneTime,
    Day,
    Week,
    Month,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsSmartAccountSigner {
    pub(crate) key: Pubkey,
    pub(crate) permissions: SquadsPermissions,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsPermissions {
    pub(crate) mask: u8,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
pub(crate) enum SquadsPolicyExpirationArgs {
    Timestamp(i64),
    SettingsState,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
pub(crate) enum SquadsPolicyCreationPayload {
    InternalFundTransfer(Vec<u8>),
    SpendingLimit(SquadsSpendingLimitPolicyCreationPayload),
    SettingsChange(Vec<u8>),
    LegacyProgramInteraction(SquadsProgramInteractionPolicyCreationPayloadLegacy),
    ProgramInteraction(SquadsProgramInteractionPolicyCreationPayload),
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsProgramInteractionPolicyCreationPayloadLegacy {
    pub(crate) account_index: u8,
    pub(crate) instructions_constraints: Vec<SquadsInstructionConstraint>,
    pub(crate) pre_hook: Option<SquadsHook>,
    pub(crate) post_hook: Option<SquadsHook>,
    pub(crate) spending_limits: Vec<SquadsLimitedSpendingLimit>,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsProgramInteractionPolicyCreationPayload {
    pub(crate) account_index: u8,
    pub(crate) pubkey_table: SquadsSmallVec<Pubkey>,
    pub(crate) instructions_constraints: SquadsSmallVec<SquadsCompiledInstructionConstraint>,
    pub(crate) pre_hook: Option<SquadsCompiledHook>,
    pub(crate) post_hook: Option<SquadsCompiledHook>,
    pub(crate) spending_limits: SquadsSmallVec<SquadsCompiledLimitedSpendingLimit>,
}

#[derive(Clone)]
pub(crate) struct SquadsSmallVec<T>(pub(crate) Vec<T>);

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
pub(crate) struct SquadsCompiledInstructionConstraint {
    pub(crate) program_id_index: u8,
    pub(crate) account_constraints: SquadsSmallVec<SquadsCompiledAccountConstraint>,
    pub(crate) data_constraints: SquadsSmallVec<SquadsDataConstraint>,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsCompiledAccountConstraint {
    pub(crate) account_index: u8,
    pub(crate) account_constraint: SquadsCompiledAccountConstraintType,
    pub(crate) owner_index: Option<u8>,
}

#[derive(BorshSerialize)]
pub(crate) enum SquadsCompiledAccountConstraintType {
    Pubkey(SquadsSmallVec<u8>),
    AccountData(SquadsSmallVec<SquadsDataConstraint>),
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsCompiledHook {
    pub(crate) num_extra_accounts: u8,
    pub(crate) account_constraints: SquadsSmallVec<SquadsCompiledAccountConstraint>,
    pub(crate) instruction_data: SquadsSmallVec<u8>,
    pub(crate) program_id_index: u8,
    pub(crate) pass_inner_instructions: bool,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsCompiledLimitedSpendingLimit {
    pub(crate) mint_index: u8,
    pub(crate) time_constraints: SquadsLimitedTimeConstraints,
    pub(crate) quantity_constraints: SquadsLimitedQuantityConstraints,
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsInstructionConstraint {
    pub(crate) program_id: Pubkey,
    pub(crate) account_constraints: Vec<SquadsAccountConstraint>,
    pub(crate) data_constraints: Vec<SquadsDataConstraint>,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsHook {
    pub(crate) num_extra_accounts: u8,
    pub(crate) account_constraints: Vec<SquadsAccountConstraint>,
    pub(crate) instruction_data: Vec<u8>,
    pub(crate) program_id: Pubkey,
    pub(crate) pass_inner_instructions: bool,
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsAccountConstraint {
    pub(crate) account_index: u8,
    pub(crate) account_constraint: SquadsAccountConstraintType,
    pub(crate) owner: Option<Pubkey>,
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
pub(crate) enum SquadsAccountConstraintType {
    Pubkey(Vec<Pubkey>),
    AccountData(Vec<SquadsDataConstraint>),
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsDataConstraint {
    pub(crate) data_offset: u64,
    pub(crate) data_value: SquadsDataValue,
    pub(crate) operator: SquadsDataOperator,
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
pub(crate) enum SquadsDataValue {
    U8(u8),
    U16Le(u16),
    U32Le(u32),
    U64Le(u64),
    U128Le(u128),
    U8Slice(Vec<u8>),
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
pub(crate) enum SquadsDataOperator {
    Equals,
    NotEquals,
    GreaterThan,
    GreaterThanOrEqualTo,
    LessThan,
    LessThanOrEqualTo,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsLimitedSpendingLimit {
    pub(crate) mint: Pubkey,
    pub(crate) time_constraints: SquadsLimitedTimeConstraints,
    pub(crate) quantity_constraints: SquadsLimitedQuantityConstraints,
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsLimitedTimeConstraints {
    pub(crate) start: i64,
    pub(crate) expiration: Option<i64>,
    pub(crate) period: SquadsPeriodV2,
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsLimitedQuantityConstraints {
    pub(crate) max_per_period: u64,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsSpendingLimitPolicyCreationPayload {
    pub(crate) mint: Pubkey,
    pub(crate) source_account_index: u8,
    pub(crate) time_constraints: SquadsTimeConstraints,
    pub(crate) quantity_constraints: SquadsQuantityConstraints,
    pub(crate) usage_state: Option<SquadsUsageState>,
    pub(crate) destinations: Vec<Pubkey>,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsTimeConstraints {
    pub(crate) start: i64,
    pub(crate) expiration: Option<i64>,
    pub(crate) period: SquadsPeriodV2,
    pub(crate) accumulate_unused: bool,
}

#[derive(BorshSerialize, Clone)]
#[allow(dead_code)]
pub(crate) enum SquadsPeriodV2 {
    OneTime,
    Daily,
    Weekly,
    Monthly,
    Custom(i64),
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsQuantityConstraints {
    pub(crate) max_per_period: u64,
    pub(crate) max_per_use: u64,
    pub(crate) enforce_exact_quantity: bool,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsUsageState {
    pub(crate) remaining_in_period: u64,
    pub(crate) last_reset: i64,
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsSyncSettingsTransactionArgs {
    pub(crate) num_signers: u8,
    pub(crate) actions: Vec<SquadsSettingsAction>,
    pub(crate) memo: Option<String>,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
pub(crate) enum SquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(SquadsPolicyPayload),
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsSyncTransactionArgs {
    pub(crate) account_index: u8,
    pub(crate) num_signers: u8,
    pub(crate) payload: SquadsSyncPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
pub(crate) enum SquadsPolicyPayload {
    InternalFundTransfer(Vec<u8>),
    ProgramInteraction(SquadsProgramInteractionPayload),
    SpendingLimit(SquadsSpendingLimitPayload),
    SettingsChange(Vec<u8>),
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsProgramInteractionPayload {
    pub(crate) instruction_constraint_indices: Option<Vec<u8>>,
    pub(crate) transaction_payload: SquadsProgramInteractionTransactionPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
pub(crate) enum SquadsProgramInteractionTransactionPayload {
    AsyncTransaction(Vec<u8>),
    SyncTransaction(SquadsProgramInteractionSyncPayload),
}

#[derive(BorshSerialize)]
pub(crate) struct SquadsProgramInteractionSyncPayload {
    pub(crate) account_index: u8,
    pub(crate) instructions: Vec<u8>,
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
pub(crate) struct SquadsSpendingLimitPayload {
    pub(crate) amount: u64,
    pub(crate) destination: Pubkey,
    pub(crate) decimals: u8,
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
