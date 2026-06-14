use crate::*;
use borsh::BorshSerialize;
use solana_sdk::{
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::fmt;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LoyalActionError {
    EmptySwapLanes,
    EmptyStableMints,
    EmptyKaminoMarkets,
    EmptyKaminoLiquidityMints,
    EmptyRebalanceTransfers,
    EmptyAllowedMints,
    DuplicateActionSeeds,
    PubkeyTableOverflow,
    InvalidFeeBps,
    InvalidAllowedMintCount,
    InvalidLaneCount,
    InvalidRebalanceTransferCount,
    MissingActionStep,
    SplitActionRoute,
}

impl fmt::Display for LoyalActionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::EmptySwapLanes => formatter.write_str("at least one swap lane is required"),
            Self::EmptyStableMints => formatter.write_str("at least one stable mint is required"),
            Self::EmptyKaminoMarkets => {
                formatter.write_str("at least one Kamino market is required")
            }
            Self::EmptyKaminoLiquidityMints => {
                formatter.write_str("at least one Kamino liquidity mint is required")
            }
            Self::EmptyRebalanceTransfers => {
                formatter.write_str("at least one Loyal Hub rebalance transfer is required")
            }
            Self::EmptyAllowedMints => {
                formatter.write_str("at least one Loyal Hub allowed mint is required")
            }
            Self::DuplicateActionSeeds => formatter.write_str("action seeds must be distinct"),
            Self::PubkeyTableOverflow => {
                formatter.write_str("Squads ProgramInteraction pubkey table overflow")
            }
            Self::InvalidFeeBps => write!(
                formatter,
                "fee basis points must be <= {}",
                loyal_hub_abi::MAX_FEE_BPS
            ),
            Self::InvalidAllowedMintCount => {
                write!(
                    formatter,
                    "Loyal Hub supports 1..={} allowed mints",
                    loyal_hub_abi::MAX_ALLOWED_MINTS
                )
            }
            Self::InvalidLaneCount => formatter.write_str("Loyal Hub supports at least one lane"),
            Self::InvalidRebalanceTransferCount => {
                write!(
                    formatter,
                    "Loyal Hub rebalance supports 1..={} transfers per mint",
                    loyal_hub_abi::MAX_REBALANCE_TRANSFERS
                )
            }
            Self::MissingActionStep => {
                formatter.write_str("requested action step is not available")
            }
            Self::SplitActionRoute => {
                formatter.write_str("route steps must share one Loyal action account")
            }
        }
    }
}

impl std::error::Error for LoyalActionError {}

pub type Result<T> = std::result::Result<T, LoyalActionError>;

pub fn derive_action_account(squads_settings: &Pubkey, action_seed: u64) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_POLICY,
            squads_settings.as_ref(),
            &action_seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsCompiledInstruction {
    pub program_id_index: u8,
    pub accounts: Vec<u8>,
    pub data: Vec<u8>,
}

pub fn compile_squads_inner_instruction(
    transaction_accounts: &mut Vec<AccountMeta>,
    instruction: Instruction,
) -> SquadsCompiledInstruction {
    let accounts = instruction
        .accounts
        .into_iter()
        .map(|account| push_or_update_account_meta(transaction_accounts, account))
        .collect();
    let program_id_index = push_or_update_account_meta(
        transaction_accounts,
        AccountMeta::new_readonly(instruction.program_id, false),
    );

    SquadsCompiledInstruction {
        program_id_index,
        accounts,
        data: instruction.data,
    }
}

pub fn execute_program_interaction_policy_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    for account in &mut transaction_accounts {
        account.is_signer = false;
    }
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

fn push_or_update_account_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> u8 {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_writable |= meta.is_writable;
        accounts[index].is_signer |= meta.is_signer;
        return index
            .try_into()
            .expect("Squads account index should fit in u8");
    }

    let index = accounts.len();
    accounts.push(meta);
    index
        .try_into()
        .expect("Squads account index should fit in u8")
}

fn squads_compiled_instruction_payload(instructions: &[SquadsCompiledInstruction]) -> Vec<u8> {
    let mut payload = Vec::new();
    payload.push(
        instructions
            .len()
            .try_into()
            .expect("Squads sync payload supports up to 255 instructions"),
    );

    for instruction in instructions {
        payload.push(instruction.program_id_index);
        payload.push(
            instruction
                .accounts
                .len()
                .try_into()
                .expect("account index count fits in u8"),
        );
        payload.extend_from_slice(&instruction.accounts);
        payload.extend_from_slice(&(instruction.data.len() as u16).to_le_bytes());
        payload.extend_from_slice(&instruction.data);
    }

    payload
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

pub(crate) fn create_program_interaction_action_instruction(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    action_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Result<Instruction> {
    let (action_account, _) = derive_action_account(&settings, action_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: action_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::ProgramInteraction(
            compile_program_interaction_payload(account_index, constraints)?,
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

    Ok(Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(action_account, false),
        ],
        data: serialize_settings_actions(vec![action]),
    })
}

pub(crate) fn update_program_interaction_action_instruction(
    settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Result<Instruction> {
    let action = SquadsSettingsAction::PolicyUpdate {
        policy,
        policy_update_payload: SquadsPolicyCreationPayload::ProgramInteraction(
            compile_program_interaction_payload(account_index, constraints)?,
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        expiration_args: None,
    };

    Ok(Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_settings_actions(vec![action]),
    })
}

fn compile_program_interaction_payload(
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Result<SquadsProgramInteractionPolicyCreationPayload> {
    Ok(SquadsProgramInteractionPolicyCreationPayload {
        account_index,
        instructions_constraints: constraints,
        pre_hook: None,
        post_hook: None,
        spending_limits: Vec::new(),
    })
}

fn serialize_settings_actions(actions: Vec<SquadsSettingsAction>) -> Vec<u8> {
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

fn anchor_instruction_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("global:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().unwrap()
}

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
    SpendingLimit(Vec<u8>),
    SettingsChange(Vec<u8>),
    ProgramInteraction(SquadsProgramInteractionPolicyCreationPayload),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPolicyCreationPayload {
    account_index: u8,
    instructions_constraints: Vec<SquadsInstructionConstraint>,
    pre_hook: Option<SquadsHook>,
    post_hook: Option<SquadsHook>,
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
}

#[derive(BorshSerialize)]
struct SquadsHook {
    num_extra_accounts: u8,
    account_constraints: Vec<SquadsAccountConstraint>,
    instruction_data: Vec<u8>,
    program_id: Pubkey,
    pass_inner_instructions: bool,
}

#[derive(BorshSerialize)]
struct SquadsLimitedSpendingLimit {
    mint: Pubkey,
    time_constraints: SquadsLimitedTimeConstraints,
    quantity_constraints: SquadsLimitedQuantityConstraints,
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsInstructionConstraint {
    pub(crate) program_id: Pubkey,
    pub(crate) account_constraints: Vec<SquadsAccountConstraint>,
    pub(crate) data_constraints: Vec<SquadsDataConstraint>,
}

#[derive(BorshSerialize, Clone)]
pub(crate) struct SquadsAccountConstraint {
    pub(crate) account_index: u8,
    pub(crate) account_constraint: SquadsAccountConstraintType,
    pub(crate) owner: Option<Pubkey>,
}

#[derive(BorshSerialize, Clone)]
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
struct SquadsLimitedTimeConstraints {
    start: i64,
    expiration: Option<i64>,
    period: SquadsPeriodV2,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPeriodV2 {
    OneTime,
}

#[derive(BorshSerialize)]
struct SquadsLimitedQuantityConstraints {
    max_per_period: u64,
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
    InternalFundTransfer(SquadsInternalFundTransferPayload),
    ProgramInteraction(SquadsProgramInteractionPayload),
    SpendingLimit(SquadsSpendingLimitPayload),
    SettingsChange(Vec<u8>),
}

#[derive(BorshSerialize)]
struct SquadsInternalFundTransferPayload {
    source_index: u8,
    destination_index: u8,
    mint: Pubkey,
    decimals: u8,
    amount: u64,
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

#[derive(BorshSerialize)]
struct SquadsSpendingLimitPayload {
    amount: u64,
    destination: Pubkey,
    decimals: u8,
}

#[derive(BorshSerialize)]
struct SquadsSyncSettingsTransactionArgs {
    num_signers: u8,
    actions: Vec<SquadsSettingsAction>,
    memo: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derives_action_account_with_squads_policy_seed() {
        let settings = Pubkey::new_unique();
        let seed = 7;

        assert_eq!(
            derive_action_account(&settings, seed),
            Pubkey::find_program_address(
                &[
                    SQUADS_SEED_PREFIX,
                    SQUADS_SEED_POLICY,
                    settings.as_ref(),
                    &seed.to_le_bytes(),
                ],
                &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            )
        );
    }

    #[test]
    fn compiles_inner_instruction_into_squads_account_indexes() {
        let shared = Pubkey::new_unique();
        let other = Pubkey::new_unique();
        let program = Pubkey::new_unique();
        let mut transaction_accounts = vec![AccountMeta::new_readonly(shared, false)];

        let compiled = compile_squads_inner_instruction(
            &mut transaction_accounts,
            Instruction {
                program_id: program,
                accounts: vec![
                    AccountMeta::new(shared, true),
                    AccountMeta::new_readonly(other, false),
                ],
                data: vec![9, 8, 7],
            },
        );

        assert_eq!(
            compiled,
            SquadsCompiledInstruction {
                program_id_index: 2,
                accounts: vec![0, 1],
                data: vec![9, 8, 7],
            }
        );
        assert_eq!(transaction_accounts.len(), 3);
        assert_eq!(transaction_accounts[0].pubkey, shared);
        assert!(transaction_accounts[0].is_writable);
        assert!(transaction_accounts[0].is_signer);
        assert_eq!(
            transaction_accounts[1],
            AccountMeta::new_readonly(other, false)
        );
        assert_eq!(
            transaction_accounts[2],
            AccountMeta::new_readonly(program, false)
        );
    }

    #[test]
    fn builds_program_interaction_policy_instruction() {
        let policy = Pubkey::new_unique();
        let signer = Pubkey::new_unique();
        let extra_account = Pubkey::new_unique();
        let vault_signer = Pubkey::new_unique();

        let instruction = execute_program_interaction_policy_instruction(
            policy,
            signer,
            1,
            vec![SquadsCompiledInstruction {
                program_id_index: 0,
                accounts: vec![1],
                data: vec![9, 8],
            }],
            vec![0, 2],
            vec![
                AccountMeta::new(extra_account, false),
                AccountMeta::new(vault_signer, true),
            ],
        );

        assert_eq!(instruction.program_id, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(
            instruction.accounts,
            vec![
                AccountMeta::new(policy, false),
                AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
                AccountMeta::new_readonly(signer, true),
                AccountMeta::new(extra_account, false),
                AccountMeta::new(vault_signer, false),
            ]
        );
        assert!(instruction
            .data
            .starts_with(&SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR));
        assert_eq!(instruction.data[8], 1);
        assert_eq!(instruction.data[9], SQUADS_SYNC_SIGNER_COUNT);
        assert_eq!(instruction.data[10], 1);
        assert_eq!(instruction.data[11], 1);
        assert_eq!(instruction.data[12], 1);
        assert_eq!(&instruction.data[13..17], &[2, 0, 0, 0]);
        assert_eq!(&instruction.data[17..19], &[0, 2]);
        assert_eq!(instruction.data[19], 1);
        assert_eq!(instruction.data[20], 1);
        assert_eq!(&instruction.data[21..25], &[8, 0, 0, 0]);
        assert_eq!(&instruction.data[25..], &[1, 0, 1, 1, 2, 0, 9, 8]);
    }
}
