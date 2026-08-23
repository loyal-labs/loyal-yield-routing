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
    EmptyStablecoinPairs,
    EmptyStableMints,
    EmptyKaminoMarkets,
    EmptyKaminoLiquidityMints,
    EmptyRebalanceTransfers,
    EmptyAllowedMints,
    DuplicateActionSeeds,
    PubkeyTableOverflow,
    CompactPolicyPayloadOverflow,
    InvalidFeeBps,
    InvalidHubAdmin,
    InvalidAllowedMintCount,
    InvalidLaneCount,
    InvalidRebalanceTransferCount,
    MissingActionStep,
    SplitActionRoute,
    InvalidSettingsHandoff,
    InvalidStablecoinPair,
    DuplicateStablecoinPair,
    TooManyStablecoinPairs,
    InvalidSlippageBps,
    InvalidDailySpendingCap,
    InvalidPolicyConstraint,
}

impl fmt::Display for LoyalActionError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::EmptySwapLanes => formatter.write_str("at least one swap lane is required"),
            Self::EmptyStablecoinPairs => {
                formatter.write_str("at least one directed stablecoin pair is required")
            }
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
            Self::CompactPolicyPayloadOverflow => {
                formatter.write_str("Squads compact ProgramInteraction payload overflow")
            }
            Self::InvalidFeeBps => write!(
                formatter,
                "fee basis points must be <= {}",
                loyal_hub_abi::MAX_FEE_BPS
            ),
            Self::InvalidHubAdmin => {
                formatter.write_str("Loyal Hub config payer must be the configured admin")
            }
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
            Self::InvalidSettingsHandoff => {
                formatter.write_str("invalid atomic Squads Settings signer handoff")
            }
            Self::InvalidStablecoinPair => formatter
                .write_str("directed stablecoin pairs must use two different canonical Earn mints"),
            Self::DuplicateStablecoinPair => {
                formatter.write_str("directed stablecoin pairs must be unique")
            }
            Self::TooManyStablecoinPairs => formatter.write_str(
                "directed stablecoin pairs exceed the Squads 20-constraint policy limit",
            ),
            Self::InvalidSlippageBps => {
                formatter.write_str("slippage basis points must be at most 10000")
            }
            Self::InvalidDailySpendingCap => {
                formatter.write_str("daily source-mint spending cap must be positive")
            }
            Self::InvalidPolicyConstraint => {
                formatter.write_str("exact ProgramInteraction policy constraint is invalid")
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

pub fn derive_squads_vault(squads_settings: &Pubkey, vault_index: u8) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            squads_settings.as_ref(),
            SQUADS_SEED_PREFIX,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_v4_vault(multisig: &Pubkey, vault_index: u8) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[b"multisig", multisig.as_ref(), b"vault", &[vault_index]],
        &SQUADS_V4_PROGRAM_ID,
    )
}

pub fn derive_classic_associated_token_account(owner: Pubkey, mint: Pubkey) -> Pubkey {
    derive_associated_token_account(owner, mint, spl_token::id())
}

pub fn derive_associated_token_account(
    owner: Pubkey,
    mint: Pubkey,
    token_program: Pubkey,
) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsCompiledInstruction {
    pub program_id_index: u8,
    pub accounts: Vec<u8>,
    pub data: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsSettingsSignerHandoff {
    pub settings: Pubkey,
    pub current_signer: Pubkey,
    pub new_signer: Pubkey,
    pub new_signer_permissions_mask: u8,
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

pub fn execute_sync_transaction_instruction(
    settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    for account in &mut transaction_accounts {
        account.is_signer = false;
    }
    let mut accounts = vec![
        AccountMeta::new(settings, false),
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

pub fn remove_policy_instruction(
    settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
) -> Instruction {
    remove_policies_instruction(settings, authority, &[policy])
}

pub fn remove_policies_instruction(
    settings: Pubkey,
    authority: Pubkey,
    policies: &[Pubkey],
) -> Instruction {
    let actions = policies
        .iter()
        .copied()
        .map(|policy| SquadsSettingsAction::PolicyRemove { policy })
        .collect();
    let mut accounts = vec![
        AccountMeta::new(settings, false),
        AccountMeta::new(authority, true),
        AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(authority, true),
    ];
    accounts.extend(
        policies
            .iter()
            .copied()
            .map(|policy| AccountMeta::new(policy, false)),
    );

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_settings_actions(actions),
    }
}

pub fn update_exact_program_interaction_policy_instruction(
    settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    instructions: &[Instruction],
    pinned_account_indices: &[Vec<u8>],
) -> Result<Instruction> {
    let constraints = exact_program_interaction_constraints(instructions, pinned_account_indices)?;
    update_program_interaction_action_instruction(
        settings,
        authority,
        policy,
        delegated_signer,
        account_index,
        constraints,
    )
}

#[derive(Clone, Debug)]
pub struct SemanticProgramInteractionConstraint {
    pub program_id: Pubkey,
    pub account_pubkeys: Vec<(u8, Vec<Pubkey>)>,
    pub data: Vec<SemanticProgramInteractionDataConstraint>,
}

#[derive(Clone, Debug)]
pub enum SemanticProgramInteractionDataConstraint {
    SliceEquals { offset: u64, value: Vec<u8> },
    U8Equals { offset: u64, value: u8 },
    U16LessThanOrEqual { offset: u64, value: u16 },
    U32Equals { offset: u64, value: u32 },
}

/// Update a long-lived hookless policy from a small semantic contract. This is
/// intentionally the only generic surface: callers name protocol programs,
/// fixed custody accounts, and bounded wire fields; hooks, account-data
/// predicates, spending limits, and expirations cannot be expressed here.
pub fn update_semantic_program_interaction_policy_instruction(
    settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    specs: Vec<SemanticProgramInteractionConstraint>,
) -> Result<Instruction> {
    let constraints = semantic_program_interaction_constraints(specs)?;
    update_program_interaction_action_instruction(
        settings,
        authority,
        policy,
        delegated_signer,
        account_index,
        constraints,
    )
}

pub fn create_semantic_program_interaction_policy_instruction(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    account_index: u8,
    specs: Vec<SemanticProgramInteractionConstraint>,
) -> Result<Instruction> {
    let constraints = semantic_program_interaction_constraints(specs)?;
    create_program_interaction_action_instruction(
        settings,
        authority,
        delegated_signer,
        policy_seed,
        account_index,
        constraints,
    )
}

fn semantic_program_interaction_constraints(
    specs: Vec<SemanticProgramInteractionConstraint>,
) -> Result<Vec<SquadsInstructionConstraint>> {
    if specs.is_empty() {
        return Err(LoyalActionError::InvalidPolicyConstraint);
    }
    specs
        .into_iter()
        .map(|spec| {
            let mut seen = std::collections::BTreeSet::new();
            let account_constraints = spec
                .account_pubkeys
                .into_iter()
                .map(|(account_index, pubkeys)| {
                    if pubkeys.is_empty() || !seen.insert(account_index) {
                        return Err(LoyalActionError::InvalidPolicyConstraint);
                    }
                    Ok(SquadsAccountConstraint {
                        account_index,
                        account_constraint: SquadsAccountConstraintType::Pubkey(pubkeys),
                        owner: None,
                    })
                })
                .collect::<Result<Vec<_>>>()?;
            let data_constraints = semantic_data_constraints(spec.data)?;
            Ok(SquadsInstructionConstraint {
                program_id: spec.program_id,
                account_constraints,
                data_constraints,
            })
        })
        .collect()
}

fn semantic_data_constraints(
    specs: Vec<SemanticProgramInteractionDataConstraint>,
) -> Result<Vec<SquadsDataConstraint>> {
    let constraints = specs
        .into_iter()
        .map(|constraint| match constraint {
            SemanticProgramInteractionDataConstraint::SliceEquals { offset, value }
                if !value.is_empty() =>
            {
                Ok(SquadsDataConstraint {
                    data_offset: offset,
                    data_value: SquadsDataValue::U8Slice(value),
                    operator: SquadsDataOperator::Equals,
                })
            }
            SemanticProgramInteractionDataConstraint::U8Equals { offset, value } => {
                Ok(SquadsDataConstraint {
                    data_offset: offset,
                    data_value: SquadsDataValue::U8(value),
                    operator: SquadsDataOperator::Equals,
                })
            }
            SemanticProgramInteractionDataConstraint::U16LessThanOrEqual { offset, value }
                if value > 0 =>
            {
                Ok(SquadsDataConstraint {
                    data_offset: offset,
                    data_value: SquadsDataValue::U16Le(value),
                    operator: SquadsDataOperator::LessThanOrEqualTo,
                })
            }
            SemanticProgramInteractionDataConstraint::U32Equals { offset, value } => {
                Ok(SquadsDataConstraint {
                    data_offset: offset,
                    data_value: SquadsDataValue::U32Le(value),
                    operator: SquadsDataOperator::Equals,
                })
            }
            _ => Err(LoyalActionError::InvalidPolicyConstraint),
        })
        .collect::<Result<Vec<_>>>()?;
    if constraints.is_empty() {
        return Err(LoyalActionError::InvalidPolicyConstraint);
    }
    Ok(constraints)
}

fn exact_program_interaction_constraints(
    instructions: &[Instruction],
    pinned_account_indices: &[Vec<u8>],
) -> Result<Vec<SquadsInstructionConstraint>> {
    if instructions.is_empty() || instructions.len() != pinned_account_indices.len() {
        return Err(LoyalActionError::InvalidPolicyConstraint);
    }
    instructions
        .iter()
        .zip(pinned_account_indices)
        .map(|(instruction, pins)| {
            let mut seen = std::collections::BTreeSet::new();
            let account_constraints = pins
                .iter()
                .copied()
                .map(|index| {
                    if !seen.insert(index) {
                        return Err(LoyalActionError::InvalidPolicyConstraint);
                    }
                    let account = instruction
                        .accounts
                        .get(usize::from(index))
                        .ok_or(LoyalActionError::InvalidPolicyConstraint)?;
                    Ok(SquadsAccountConstraint {
                        account_index: index,
                        account_constraint: SquadsAccountConstraintType::Pubkey(vec![
                            account.pubkey,
                        ]),
                        owner: None,
                    })
                })
                .collect::<Result<Vec<_>>>()?;
            Ok(SquadsInstructionConstraint {
                program_id: instruction.program_id,
                account_constraints,
                data_constraints: vec![SquadsDataConstraint {
                    data_offset: 0,
                    data_value: SquadsDataValue::U8Slice(instruction.data.clone()),
                    operator: SquadsDataOperator::Equals,
                }],
            })
        })
        .collect()
}

/// Atomically installs a full-permission Settings signer before removing the
/// current signer. The add-first order prevents a threshold-1 Settings account
/// from passing through an empty signer set.
pub fn handoff_settings_signer_instruction(
    settings: Pubkey,
    current_signer: Pubkey,
    new_signer: Pubkey,
) -> Result<Instruction> {
    if settings == Pubkey::default()
        || current_signer == Pubkey::default()
        || new_signer == Pubkey::default()
        || current_signer == new_signer
    {
        return Err(LoyalActionError::InvalidSettingsHandoff);
    }
    let actions = vec![
        SquadsSettingsAction::AddSigner {
            new_signer: SquadsSmartAccountSigner {
                key: new_signer,
                permissions: SquadsPermissions {
                    mask: SQUADS_FULL_PERMISSIONS_MASK,
                },
            },
        },
        SquadsSettingsAction::RemoveSigner {
            old_signer: current_signer,
        },
    ];

    Ok(Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(settings, false),
            AccountMeta::new(current_signer, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(current_signer, true),
        ],
        data: serialize_settings_actions(actions),
    })
}

/// Independently decodes the narrow handoff wire shape emitted above. This is
/// intentionally not a general Settings-action decoder: any extra action,
/// reordered action, memo, account, or trailing byte is rejected.
pub fn decode_settings_signer_handoff_instruction(
    instruction: &Instruction,
) -> Result<SquadsSettingsSignerHandoff> {
    let accounts = &instruction.accounts;
    if instruction.program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID
        || accounts.len() != 5
        || accounts[0].is_signer
        || !accounts[0].is_writable
        || !accounts[1].is_signer
        || !accounts[1].is_writable
        || accounts[2] != AccountMeta::new_readonly(solana_sdk::system_program::ID, false)
        || accounts[3] != AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false)
        || accounts[4] != AccountMeta::new_readonly(accounts[1].pubkey, true)
    {
        return Err(LoyalActionError::InvalidSettingsHandoff);
    }

    let data = &instruction.data;
    let discriminator = anchor_instruction_discriminator("execute_settings_transaction_sync");
    if data.len() != 81
        || data[..8] != discriminator
        || data[8] != SQUADS_SYNC_SIGNER_COUNT
        || u32::from_le_bytes(
            data[9..13]
                .try_into()
                .map_err(|_| LoyalActionError::InvalidSettingsHandoff)?,
        ) != 2
        || data[13] != 0
        || data[46] != SQUADS_FULL_PERMISSIONS_MASK
        || data[47] != 1
        || data[80] != 0
    {
        return Err(LoyalActionError::InvalidSettingsHandoff);
    }
    let new_signer = Pubkey::new_from_array(
        data[14..46]
            .try_into()
            .map_err(|_| LoyalActionError::InvalidSettingsHandoff)?,
    );
    let removed_signer = Pubkey::new_from_array(
        data[48..80]
            .try_into()
            .map_err(|_| LoyalActionError::InvalidSettingsHandoff)?,
    );
    if new_signer == Pubkey::default()
        || removed_signer != accounts[1].pubkey
        || new_signer == removed_signer
    {
        return Err(LoyalActionError::InvalidSettingsHandoff);
    }

    Ok(SquadsSettingsSignerHandoff {
        settings: accounts[0].pubkey,
        current_signer: removed_signer,
        new_signer,
        new_signer_permissions_mask: data[46],
    })
}

/// Creates the exact effectively-unlimited SPL SpendingLimit used by an
/// autonomous vault to return one mint to one treasury owner.
#[allow(clippy::too_many_arguments)]
pub fn create_unlimited_spl_spending_limit_policy_instruction(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    source_account_index: u8,
    mint: Pubkey,
    destination_owner: Pubkey,
) -> Instruction {
    let (policy, _) = derive_action_account(&settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::SpendingLimit(
            SquadsSpendingLimitPolicyCreationPayload {
                mint,
                source_account_index,
                time_constraints: SquadsTimeConstraints {
                    start: 0,
                    expiration: None,
                    period: SquadsPeriodV2::OneTime,
                    accumulate_unused: false,
                },
                quantity_constraints: SquadsQuantityConstraints {
                    max_per_period: u64::MAX,
                    max_per_use: 0,
                    enforce_exact_quantity: false,
                },
                usage_state: None,
                destinations: vec![destination_owner],
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
            AccountMeta::new(settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_settings_actions(vec![action]),
    }
}

/// Executes an SPL SpendingLimit through canonical classic-token ATAs. The
/// on-chain policy ultimately authorizes the destination owner and mint; using
/// canonical ATAs here removes client-side destination ambiguity.
#[allow(clippy::too_many_arguments)]
pub fn execute_spl_spending_limit_policy_instruction(
    policy: Pubkey,
    signer: Pubkey,
    settings: Pubkey,
    source_account_index: u8,
    mint: Pubkey,
    destination_owner: Pubkey,
    amount: u64,
    decimals: u8,
) -> Instruction {
    let vault = derive_squads_vault(&settings, source_account_index).0;
    let source_token_account = derive_classic_associated_token_account(vault, mint);
    let destination_token_account =
        derive_classic_associated_token_account(destination_owner, mint);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(policy, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new_readonly(vault, false),
            AccountMeta::new(source_token_account, false),
            AccountMeta::new(destination_token_account, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: serialize_squads_sync_policy_payload_args(
            source_account_index,
            SquadsPolicyPayload::SpendingLimit(SquadsSpendingLimitPayload {
                amount,
                destination: destination_owner,
                decimals,
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

fn serialize_squads_sync_transaction_args(account_index: u8, payload: Vec<u8>) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Transaction(payload),
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
    create_program_interaction_action_instruction_with_spending_limits(
        settings,
        authority,
        delegated_signer,
        action_seed,
        account_index,
        constraints,
        Vec::new(),
    )
}

pub(crate) fn create_program_interaction_action_instruction_with_daily_spending_limits(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    action_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
    daily_spending_limits: &[(Pubkey, u64)],
) -> Result<Instruction> {
    create_program_interaction_action_instruction_with_spending_limits(
        settings,
        authority,
        delegated_signer,
        action_seed,
        account_index,
        constraints,
        daily_spending_limits
            .iter()
            .map(|(mint, max_per_period)| daily_spending_limit(*mint, *max_per_period))
            .collect(),
    )
}

fn create_program_interaction_action_instruction_with_spending_limits(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    action_seed: u64,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
) -> Result<Instruction> {
    let (action_account, _) = derive_action_account(&settings, action_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: action_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::LegacyProgramInteraction(
            compile_program_interaction_payload(account_index, constraints, spending_limits)?,
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
    update_program_interaction_action_instruction_with_spending_limits(
        settings,
        authority,
        policy,
        delegated_signer,
        account_index,
        constraints,
        Vec::new(),
    )
}

fn update_program_interaction_action_instruction_with_spending_limits(
    settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
    delegated_signer: Pubkey,
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
) -> Result<Instruction> {
    let action = SquadsSettingsAction::PolicyUpdate {
        policy,
        policy_update_payload: SquadsPolicyCreationPayload::ProgramInteraction(
            compile_compact_program_interaction_payload(
                account_index,
                constraints,
                spending_limits,
            )?,
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
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
) -> Result<SquadsProgramInteractionPolicyCreationPayload> {
    Ok(SquadsProgramInteractionPolicyCreationPayload {
        account_index,
        instructions_constraints: constraints,
        pre_hook: None,
        post_hook: None,
        spending_limits,
    })
}

fn compile_compact_program_interaction_payload(
    account_index: u8,
    constraints: Vec<SquadsInstructionConstraint>,
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
) -> Result<SquadsCompactProgramInteractionPolicyCreationPayload> {
    if constraints.len() > 20 || spending_limits.len() > 10 {
        return Err(LoyalActionError::CompactPolicyPayloadOverflow);
    }
    ensure_compact_u8_len(constraints.len())?;
    ensure_compact_u8_len(spending_limits.len())?;

    let legacy = compile_program_interaction_payload(account_index, constraints, spending_limits)?;
    let mut table = SquadsCompactPubkeyTable::default();
    let instructions_constraints = legacy
        .instructions_constraints
        .into_iter()
        .map(|constraint| compile_compact_instruction_constraint(&mut table, constraint))
        .collect::<Result<Vec<_>>>()?;
    let spending_limits = legacy
        .spending_limits
        .into_iter()
        .map(|limit| compile_compact_spending_limit(&mut table, limit))
        .collect::<Result<Vec<_>>>()?;

    Ok(SquadsCompactProgramInteractionPolicyCreationPayload {
        account_index: legacy.account_index,
        pubkey_table: SquadsSmallVecU8(table.pubkeys),
        instructions_constraints: SquadsSmallVecU8(instructions_constraints),
        pre_hook: None,
        post_hook: None,
        spending_limits: SquadsSmallVecU8(spending_limits),
    })
}

#[derive(Default)]
struct SquadsCompactPubkeyTable {
    pubkeys: Vec<Pubkey>,
}

impl SquadsCompactPubkeyTable {
    fn index(&mut self, pubkey: Pubkey) -> Result<u8> {
        if let Some(index) = self
            .pubkeys
            .iter()
            .position(|candidate| candidate == &pubkey)
        {
            return u8::try_from(index).map_err(|_| LoyalActionError::PubkeyTableOverflow);
        }
        if self.pubkeys.len() >= 240 {
            return Err(LoyalActionError::PubkeyTableOverflow);
        }
        let index =
            u8::try_from(self.pubkeys.len()).map_err(|_| LoyalActionError::PubkeyTableOverflow)?;
        self.pubkeys.push(pubkey);
        Ok(index)
    }
}

fn ensure_compact_u8_len(len: usize) -> Result<()> {
    u8::try_from(len)
        .map(|_| ())
        .map_err(|_| LoyalActionError::CompactPolicyPayloadOverflow)
}

fn compile_compact_instruction_constraint(
    table: &mut SquadsCompactPubkeyTable,
    constraint: SquadsInstructionConstraint,
) -> Result<SquadsCompiledInstructionConstraint> {
    ensure_compact_u8_len(constraint.account_constraints.len())?;
    ensure_compact_u8_len(constraint.data_constraints.len())?;
    let program_id_index = table.index(constraint.program_id)?;
    let account_constraints = constraint
        .account_constraints
        .into_iter()
        .map(|constraint| compile_compact_account_constraint(table, constraint))
        .collect::<Result<Vec<_>>>()?;
    Ok(SquadsCompiledInstructionConstraint {
        program_id_index,
        account_constraints: SquadsSmallVecU8(account_constraints),
        data_constraints: SquadsSmallVecU8(constraint.data_constraints),
    })
}

fn compile_compact_account_constraint(
    table: &mut SquadsCompactPubkeyTable,
    constraint: SquadsAccountConstraint,
) -> Result<SquadsCompiledAccountConstraint> {
    let account_constraint = match constraint.account_constraint {
        SquadsAccountConstraintType::Pubkey(pubkeys) => {
            ensure_compact_u8_len(pubkeys.len())?;
            let indices = pubkeys
                .into_iter()
                .map(|pubkey| table.index(pubkey))
                .collect::<Result<Vec<_>>>()?;
            SquadsCompiledAccountConstraintType::Pubkey(SquadsSmallVecU8(indices))
        }
        SquadsAccountConstraintType::AccountData(data_constraints) => {
            ensure_compact_u8_len(data_constraints.len())?;
            SquadsCompiledAccountConstraintType::AccountData(SquadsSmallVecU8(data_constraints))
        }
    };
    let owner_index = constraint
        .owner
        .map(|owner| table.index(owner))
        .transpose()?;
    Ok(SquadsCompiledAccountConstraint {
        account_index: constraint.account_index,
        account_constraint,
        owner_index,
    })
}

fn compile_compact_spending_limit(
    table: &mut SquadsCompactPubkeyTable,
    limit: SquadsLimitedSpendingLimit,
) -> Result<SquadsCompiledLimitedSpendingLimit> {
    Ok(SquadsCompiledLimitedSpendingLimit {
        mint_index: table.index(limit.mint)?,
        time_constraints: limit.time_constraints,
        quantity_constraints: limit.quantity_constraints,
    })
}

fn daily_spending_limit(mint: Pubkey, max_per_period: u64) -> SquadsLimitedSpendingLimit {
    SquadsLimitedSpendingLimit {
        mint,
        time_constraints: SquadsLimitedTimeConstraints {
            start: 0,
            expiration: None,
            period: SquadsPeriodV2::Daily,
        },
        quantity_constraints: SquadsLimitedQuantityConstraints { max_per_period },
    }
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
    SpendingLimit(SquadsSpendingLimitPolicyCreationPayload),
    SettingsChange(Vec<u8>),
    /// The pinned Squads SBF retains the embedded-pubkey creation variant at
    /// index 3 for compatibility. PolicyUpdate deliberately rejects it.
    LegacyProgramInteraction(SquadsProgramInteractionPolicyCreationPayload),
    /// Compact indexed payload at enum index 4. Squads requires this variant
    /// when updating a ProgramInteraction policy created through the legacy
    /// compatibility path.
    ProgramInteraction(SquadsCompactProgramInteractionPolicyCreationPayload),
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
struct SquadsProgramInteractionPolicyCreationPayload {
    account_index: u8,
    instructions_constraints: Vec<SquadsInstructionConstraint>,
    pre_hook: Option<SquadsHook>,
    post_hook: Option<SquadsHook>,
    spending_limits: Vec<SquadsLimitedSpendingLimit>,
}

#[derive(BorshSerialize)]
struct SquadsCompactProgramInteractionPolicyCreationPayload {
    account_index: u8,
    pubkey_table: SquadsSmallVecU8<Pubkey>,
    instructions_constraints: SquadsSmallVecU8<SquadsCompiledInstructionConstraint>,
    pre_hook: Option<SquadsCompiledHook>,
    post_hook: Option<SquadsCompiledHook>,
    spending_limits: SquadsSmallVecU8<SquadsCompiledLimitedSpendingLimit>,
}

#[derive(BorshSerialize)]
struct SquadsCompiledInstructionConstraint {
    program_id_index: u8,
    account_constraints: SquadsSmallVecU8<SquadsCompiledAccountConstraint>,
    data_constraints: SquadsSmallVecU8<SquadsDataConstraint>,
}

#[derive(BorshSerialize)]
struct SquadsCompiledHook {
    num_extra_accounts: u8,
    account_constraints: SquadsSmallVecU8<SquadsCompiledAccountConstraint>,
    instruction_data: SquadsSmallVecU16<u8>,
    program_id_index: u8,
    pass_inner_instructions: bool,
}

#[derive(BorshSerialize)]
struct SquadsCompiledLimitedSpendingLimit {
    mint_index: u8,
    time_constraints: SquadsLimitedTimeConstraints,
    quantity_constraints: SquadsLimitedQuantityConstraints,
}

#[derive(BorshSerialize)]
struct SquadsCompiledAccountConstraint {
    account_index: u8,
    account_constraint: SquadsCompiledAccountConstraintType,
    owner_index: Option<u8>,
}

#[derive(BorshSerialize)]
enum SquadsCompiledAccountConstraintType {
    Pubkey(SquadsSmallVecU8<u8>),
    AccountData(SquadsSmallVecU8<SquadsDataConstraint>),
}

struct SquadsSmallVecU8<T>(Vec<T>);

impl<T: BorshSerialize> BorshSerialize for SquadsSmallVecU8<T> {
    fn serialize<W: std::io::Write>(&self, writer: &mut W) -> std::io::Result<()> {
        let len = u8::try_from(self.0.len()).map_err(|_| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                "Squads SmallVec<u8> length overflow",
            )
        })?;
        len.serialize(writer)?;
        for value in &self.0 {
            value.serialize(writer)?;
        }
        Ok(())
    }
}

struct SquadsSmallVecU16<T>(Vec<T>);

impl<T: BorshSerialize> BorshSerialize for SquadsSmallVecU16<T> {
    fn serialize<W: std::io::Write>(&self, writer: &mut W) -> std::io::Result<()> {
        let len = u16::try_from(self.0.len()).map_err(|_| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                "Squads SmallVec<u16> length overflow",
            )
        })?;
        len.serialize(writer)?;
        for value in &self.0 {
            value.serialize(writer)?;
        }
        Ok(())
    }
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
    Daily,
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
    fn derives_the_published_mother_v4_vault() {
        let multisig = Pubkey::try_from("Gv27nnaXR8UanJmjPZ4MLS81eqee2DfzJSv7C8PkQTEC").unwrap();
        let mother = Pubkey::try_from("AQyyTwCKemeeMu8ZPZFxrXMbVwAYTSbBhi1w4PBrhvYE").unwrap();

        assert_eq!(derive_squads_v4_vault(&multisig, 0).0, mother);
    }

    #[test]
    fn signer_handoff_is_atomic_add_then_remove_and_strictly_decoded() {
        let settings = Pubkey::new_unique();
        let current = Pubkey::new_unique();
        let mother = Pubkey::new_unique();
        let instruction =
            handoff_settings_signer_instruction(settings, current, mother).expect("handoff");

        assert_eq!(
            decode_settings_signer_handoff_instruction(&instruction).expect("decode"),
            SquadsSettingsSignerHandoff {
                settings,
                current_signer: current,
                new_signer: mother,
                new_signer_permissions_mask: SQUADS_FULL_PERMISSIONS_MASK,
            }
        );

        for offset in [8usize, 9, 13, 46, 47, 80] {
            let mut mutated = instruction.clone();
            mutated.data[offset] ^= 1;
            assert_eq!(
                decode_settings_signer_handoff_instruction(&mutated),
                Err(LoyalActionError::InvalidSettingsHandoff)
            );
        }
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
