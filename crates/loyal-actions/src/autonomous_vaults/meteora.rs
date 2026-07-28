use crate::squads::{
    create_program_interaction_action_instruction, SquadsAccountConstraint,
    SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator, SquadsDataValue,
    SquadsInstructionConstraint,
};
use crate::{derive_action_account, ASSOCIATED_TOKEN_PROGRAM_ID, USDC_MINT};
use solana_sdk::{instruction::Instruction, pubkey, pubkey::Pubkey};
use std::fmt;

pub const METEORA_DLMM_PROGRAM_ID: Pubkey = pubkey!("LBUZKhRxPF3XUpBCjp4YzTKgLccjZhTSDM9YuVaPwxo");
pub const METEORA_POOL: Pubkey = pubkey!("c29DVknA5DZUCH6U5ujo1EGfiKKXZrUk6yk56yJxLrm");
pub const METEORA_LOYAL_MINT: Pubkey = pubkey!("LYLikzBQtpa9ZgVrJsqYGQpR3cC1WMJrBHaXGrQmeta");
pub const METEORA_LOYAL_RESERVE: Pubkey = pubkey!("3EXf7KbNWrBytuGK9Y7efHYM7rt837Wg9vArjd8QXgjx");
pub const METEORA_USDC_RESERVE: Pubkey = pubkey!("75MuUNzEGpRr2cKF4QU1EVn627iYqQ2LPqGosd7dNLJ5");
pub const METEORA_EVENT_AUTHORITY: Pubkey = pubkey!("D1ZN9Wj1fRSUQfCjhvnu1hqDMT7hzjzBBpi12nVniYD6");
pub const METEORA_MEMO_PROGRAM_ID: Pubkey = pubkey!("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr");

pub const METEORA_ADD_LIQUIDITY_BY_STRATEGY2_DISCRIMINATOR: [u8; 8] =
    [3, 221, 149, 218, 111, 141, 118, 213];
pub const METEORA_REMOVE_LIQUIDITY_BY_RANGE2_DISCRIMINATOR: [u8; 8] =
    [204, 2, 195, 145, 53, 145, 145, 205];
pub const METEORA_CLAIM_FEE2_DISCRIMINATOR: [u8; 8] = [112, 191, 101, 171, 28, 144, 127, 187];

/// Canonical empty `RemainingAccountsInfo` emitted by the official Rust SDK for
/// this classic-token pair. The two approved BinArray metas follow these bytes.
pub const METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO: [u8; 4] = [0; 4];

/// Current Meteora's on-chain enum tag for the reviewed two-sided SpotBalanced
/// strategy, followed by its canonical zero parameters.
pub const METEORA_SPOT_BALANCED_STRATEGY: [u8; 65] = {
    let mut strategy = [0; 65];
    strategy[0] = 3;
    strategy
};

const MAX_ACTIVE_BIN_SLIPPAGE: u32 = 3;

#[derive(Clone, Debug)]
pub struct MeteoraPolicyPlan {
    pub policy: Pubkey,
    pub policy_seed: u64,
    pub create_instruction: Instruction,
}

#[derive(Clone, Debug)]
pub struct AutonomousMeteoraPolicies {
    pub add_liquidity: MeteoraPolicyPlan,
    pub remove_liquidity: MeteoraPolicyPlan,
    pub claim_fees: MeteoraPolicyPlan,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum MeteoraPolicyError {
    NoApprovedPositions,
    NoLowerBinArrayCandidates,
    NoUpperBinArrayCandidates,
    DuplicatePolicySeeds,
    Squads(crate::LoyalActionError),
}

impl fmt::Display for MeteoraPolicyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoApprovedPositions => {
                formatter.write_str("at least one approved Meteora position is required")
            }
            Self::NoLowerBinArrayCandidates => {
                formatter.write_str("at least one lower Meteora BinArray candidate is required")
            }
            Self::NoUpperBinArrayCandidates => {
                formatter.write_str("at least one upper Meteora BinArray candidate is required")
            }
            Self::DuplicatePolicySeeds => {
                formatter.write_str("Meteora policy seeds must be distinct")
            }
            Self::Squads(error) => write!(formatter, "Squads policy encoding failed: {error}"),
        }
    }
}

impl std::error::Error for MeteoraPolicyError {}

impl From<crate::LoyalActionError> for MeteoraPolicyError {
    fn from(error: crate::LoyalActionError) -> Self {
        Self::Squads(error)
    }
}

#[allow(clippy::too_many_arguments)]
pub fn create_meteora_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
    add_liquidity_seed: u64,
    remove_liquidity_seed: u64,
    claim_fees_seed: u64,
    mut approved_positions: Vec<Pubkey>,
    mut lower_bin_array_candidates: Vec<Pubkey>,
    mut upper_bin_array_candidates: Vec<Pubkey>,
) -> Result<AutonomousMeteoraPolicies, MeteoraPolicyError> {
    approved_positions.sort();
    approved_positions.dedup();
    if approved_positions.is_empty() {
        return Err(MeteoraPolicyError::NoApprovedPositions);
    }
    lower_bin_array_candidates.sort();
    lower_bin_array_candidates.dedup();
    if lower_bin_array_candidates.is_empty() {
        return Err(MeteoraPolicyError::NoLowerBinArrayCandidates);
    }
    upper_bin_array_candidates.sort();
    upper_bin_array_candidates.dedup();
    if upper_bin_array_candidates.is_empty() {
        return Err(MeteoraPolicyError::NoUpperBinArrayCandidates);
    }
    if add_liquidity_seed == remove_liquidity_seed
        || add_liquidity_seed == claim_fees_seed
        || remove_liquidity_seed == claim_fees_seed
    {
        return Err(MeteoraPolicyError::DuplicatePolicySeeds);
    }

    let vault_loyal = derive_vault_token_account(vault, METEORA_LOYAL_MINT);
    let vault_usdc = derive_vault_token_account(vault, USDC_MINT);
    // The approved base PositionV2 can use either of two adjacent BinArray
    // windows. Squads pins each suffix slot to its candidate set; DLMM then
    // enforces that the selected lower/upper pair matches the dynamic range.
    Ok(AutonomousMeteoraPolicies {
        add_liquidity: policy_plan(
            settings,
            authority,
            delegated_signer,
            vault_index,
            add_liquidity_seed,
            add_liquidity_constraint(
                vault,
                vault_loyal,
                vault_usdc,
                approved_positions.clone(),
                lower_bin_array_candidates.clone(),
                upper_bin_array_candidates.clone(),
            ),
        )?,
        remove_liquidity: policy_plan(
            settings,
            authority,
            delegated_signer,
            vault_index,
            remove_liquidity_seed,
            remove_liquidity_constraint(
                vault,
                vault_loyal,
                vault_usdc,
                approved_positions.clone(),
                lower_bin_array_candidates.clone(),
                upper_bin_array_candidates.clone(),
            ),
        )?,
        claim_fees: policy_plan(
            settings,
            authority,
            delegated_signer,
            vault_index,
            claim_fees_seed,
            claim_fee_constraint(
                vault,
                vault_loyal,
                vault_usdc,
                approved_positions,
                lower_bin_array_candidates,
                upper_bin_array_candidates,
            ),
        )?,
    })
}

pub fn derive_meteora_vault_token_accounts(vault: Pubkey) -> (Pubkey, Pubkey) {
    (
        derive_vault_token_account(vault, METEORA_LOYAL_MINT),
        derive_vault_token_account(vault, USDC_MINT),
    )
}

fn derive_vault_token_account(vault: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[vault.as_ref(), spl_token::id().as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn policy_plan(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault_index: u8,
    policy_seed: u64,
    constraint: SquadsInstructionConstraint,
) -> Result<MeteoraPolicyPlan, MeteoraPolicyError> {
    Ok(MeteoraPolicyPlan {
        policy: derive_action_account(&settings, policy_seed).0,
        policy_seed,
        create_instruction: create_program_interaction_action_instruction(
            settings,
            authority,
            delegated_signer,
            policy_seed,
            vault_index,
            vec![constraint],
        )?,
    })
}

fn add_liquidity_constraint(
    vault: Pubkey,
    vault_loyal: Pubkey,
    vault_usdc: Pubkey,
    positions: Vec<Pubkey>,
    lower_bin_arrays: Vec<Pubkey>,
    upper_bin_arrays: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: METEORA_DLMM_PROGRAM_ID,
        account_constraints: vec![
            pubkey_allowlist_constraint(0, positions),
            pubkey_constraint(1, METEORA_POOL),
            pubkey_constraint(2, METEORA_DLMM_PROGRAM_ID),
            pubkey_constraint(3, vault_loyal),
            pubkey_constraint(4, vault_usdc),
            pubkey_constraint(5, METEORA_LOYAL_RESERVE),
            pubkey_constraint(6, METEORA_USDC_RESERVE),
            pubkey_constraint(7, METEORA_LOYAL_MINT),
            pubkey_constraint(8, USDC_MINT),
            pubkey_constraint(9, vault),
            pubkey_constraint(10, spl_token::id()),
            pubkey_constraint(11, spl_token::id()),
            pubkey_constraint(12, METEORA_EVENT_AUTHORITY),
            pubkey_constraint(13, METEORA_DLMM_PROGRAM_ID),
            pubkey_allowlist_constraint(14, lower_bin_arrays),
            pubkey_allowlist_constraint(15, upper_bin_arrays),
        ],
        data_constraints: vec![
            data_slice_equals(0, METEORA_ADD_LIQUIDITY_BY_STRATEGY2_DISCRIMINATOR.to_vec()),
            SquadsDataConstraint {
                data_offset: 28,
                data_value: SquadsDataValue::U32Le(MAX_ACTIVE_BIN_SLIPPAGE),
                operator: SquadsDataOperator::LessThanOrEqualTo,
            },
            data_slice_equals(40, METEORA_SPOT_BALANCED_STRATEGY.to_vec()),
            data_slice_equals(105, METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO.to_vec()),
        ],
    }
}

fn remove_liquidity_constraint(
    vault: Pubkey,
    vault_loyal: Pubkey,
    vault_usdc: Pubkey,
    positions: Vec<Pubkey>,
    lower_bin_arrays: Vec<Pubkey>,
    upper_bin_arrays: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: METEORA_DLMM_PROGRAM_ID,
        account_constraints: vec![
            pubkey_allowlist_constraint(0, positions),
            pubkey_constraint(1, METEORA_POOL),
            pubkey_constraint(2, METEORA_DLMM_PROGRAM_ID),
            pubkey_constraint(3, vault_loyal),
            pubkey_constraint(4, vault_usdc),
            pubkey_constraint(5, METEORA_LOYAL_RESERVE),
            pubkey_constraint(6, METEORA_USDC_RESERVE),
            pubkey_constraint(7, METEORA_LOYAL_MINT),
            pubkey_constraint(8, USDC_MINT),
            pubkey_constraint(9, vault),
            pubkey_constraint(10, spl_token::id()),
            pubkey_constraint(11, spl_token::id()),
            pubkey_constraint(12, METEORA_MEMO_PROGRAM_ID),
            pubkey_constraint(13, METEORA_EVENT_AUTHORITY),
            pubkey_constraint(14, METEORA_DLMM_PROGRAM_ID),
            pubkey_allowlist_constraint(15, lower_bin_arrays),
            pubkey_allowlist_constraint(16, upper_bin_arrays),
        ],
        data_constraints: vec![
            data_slice_equals(0, METEORA_REMOVE_LIQUIDITY_BY_RANGE2_DISCRIMINATOR.to_vec()),
            data_slice_equals(18, METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO.to_vec()),
        ],
    }
}

fn claim_fee_constraint(
    vault: Pubkey,
    vault_loyal: Pubkey,
    vault_usdc: Pubkey,
    positions: Vec<Pubkey>,
    lower_bin_arrays: Vec<Pubkey>,
    upper_bin_arrays: Vec<Pubkey>,
) -> SquadsInstructionConstraint {
    SquadsInstructionConstraint {
        program_id: METEORA_DLMM_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, METEORA_POOL),
            pubkey_allowlist_constraint(1, positions),
            pubkey_constraint(2, vault),
            pubkey_constraint(3, METEORA_LOYAL_RESERVE),
            pubkey_constraint(4, METEORA_USDC_RESERVE),
            pubkey_constraint(5, vault_loyal),
            pubkey_constraint(6, vault_usdc),
            pubkey_constraint(7, METEORA_LOYAL_MINT),
            pubkey_constraint(8, USDC_MINT),
            pubkey_constraint(9, spl_token::id()),
            pubkey_constraint(10, spl_token::id()),
            pubkey_constraint(11, METEORA_MEMO_PROGRAM_ID),
            pubkey_constraint(12, METEORA_EVENT_AUTHORITY),
            pubkey_constraint(13, METEORA_DLMM_PROGRAM_ID),
            pubkey_allowlist_constraint(14, lower_bin_arrays),
            pubkey_allowlist_constraint(15, upper_bin_arrays),
        ],
        data_constraints: vec![
            data_slice_equals(0, METEORA_CLAIM_FEE2_DISCRIMINATOR.to_vec()),
            data_slice_equals(16, METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO.to_vec()),
        ],
    }
}

fn pubkey_constraint(account_index: u8, pubkey: Pubkey) -> SquadsAccountConstraint {
    pubkey_allowlist_constraint(account_index, vec![pubkey])
}

fn pubkey_allowlist_constraint(account_index: u8, pubkeys: Vec<Pubkey>) -> SquadsAccountConstraint {
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::Pubkey(pubkeys),
        owner: None,
    }
}

fn data_slice_equals(data_offset: u64, value: Vec<u8>) -> SquadsDataConstraint {
    SquadsDataConstraint {
        data_offset,
        data_value: SquadsDataValue::U8Slice(value),
        operator: SquadsDataOperator::Equals,
    }
}
