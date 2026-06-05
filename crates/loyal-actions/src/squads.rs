use crate::*;
use borsh::BorshSerialize;
use solana_sdk::{
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use std::{fmt, io::Write};

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

    Ok(policy_create_instruction(
        settings,
        authority,
        action_account,
        action,
    ))
}

pub(crate) fn create_legacy_program_interaction_action_instruction(
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
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            SquadsProgramInteractionPolicyCreationPayloadLegacy {
                account_index,
                instructions_constraints: constraints,
                pre_hook: None,
                post_hook: None,
                spending_limits: Vec::new(),
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

    Ok(policy_create_instruction(
        settings,
        authority,
        action_account,
        action,
    ))
}

fn policy_create_instruction(
    settings: Pubkey,
    authority: Pubkey,
    action_account: Pubkey,
    action: SquadsSettingsAction,
) -> Instruction {
    Instruction {
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
    }
}

fn compile_program_interaction_payload(
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Result<SquadsProgramInteractionPolicyCreationPayload> {
    let mut pubkey_table = Vec::new();
    let instructions_constraints = constraints
        .into_iter()
        .map(|constraint| compile_instruction_constraint(constraint, &mut pubkey_table))
        .collect::<Result<Vec<_>>>()?;

    Ok(SquadsProgramInteractionPolicyCreationPayload {
        account_index,
        pubkey_table: pubkey_table.into(),
        instructions_constraints: instructions_constraints.into(),
        pre_hook: None,
        post_hook: None,
        spending_limits: Vec::<SquadsCompiledLimitedSpendingLimit>::new().into(),
    })
}

fn compile_instruction_constraint(
    constraint: SquadsInstructionConstraint,
    pubkey_table: &mut Vec<Pubkey>,
) -> Result<SquadsCompiledInstructionConstraint> {
    Ok(SquadsCompiledInstructionConstraint {
        program_id_index: pubkey_table_index(pubkey_table, constraint.program_id)?,
        account_constraints: constraint
            .account_constraints
            .into_iter()
            .map(|account_constraint| compile_account_constraint(account_constraint, pubkey_table))
            .collect::<Result<Vec<_>>>()?
            .into(),
        data_constraints: constraint.data_constraints.into(),
    })
}

fn compile_account_constraint(
    constraint: SquadsAccountConstraint,
    pubkey_table: &mut Vec<Pubkey>,
) -> Result<SquadsCompiledAccountConstraint> {
    Ok(SquadsCompiledAccountConstraint {
        account_index: constraint.account_index,
        account_constraint: match constraint.account_constraint {
            SquadsAccountConstraintType::Pubkey(pubkeys) => {
                SquadsCompiledAccountConstraintType::Pubkey(
                    pubkeys
                        .into_iter()
                        .map(|pubkey| pubkey_table_index(pubkey_table, pubkey))
                        .collect::<Result<Vec<_>>>()?
                        .into(),
                )
            }
            SquadsAccountConstraintType::AccountData(data_constraints) => {
                SquadsCompiledAccountConstraintType::AccountData(data_constraints.into())
            }
        },
        owner_index: constraint
            .owner
            .map(|owner| pubkey_table_index(pubkey_table, owner))
            .transpose()?,
    })
}

fn pubkey_table_index(pubkey_table: &mut Vec<Pubkey>, pubkey: Pubkey) -> Result<u8> {
    if let Some(index) = pubkey_table.iter().position(|existing| *existing == pubkey) {
        return index
            .try_into()
            .map_err(|_| LoyalActionError::PubkeyTableOverflow);
    }

    if pubkey_table.len() >= 240 {
        return Err(LoyalActionError::PubkeyTableOverflow);
    }
    let index = pubkey_table.len();
    pubkey_table.push(pubkey);
    index
        .try_into()
        .map_err(|_| LoyalActionError::PubkeyTableOverflow)
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
}
