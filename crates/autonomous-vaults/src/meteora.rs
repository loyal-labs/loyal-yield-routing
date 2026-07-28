use anchor_lang::{InstructionData, ToAccountMetas};
use anyhow::{bail, Context, Result};
use loyal_actions::{
    autonomous_vaults::{
        create_meteora_policies, AutonomousMeteoraPolicies, METEORA_DLMM_PROGRAM_ID,
        METEORA_EVENT_AUTHORITY, METEORA_LOYAL_MINT, METEORA_LOYAL_RESERVE,
        METEORA_MEMO_PROGRAM_ID, METEORA_POOL, METEORA_USDC_RESERVE,
    },
    decode_squads_policy_create_actions, SquadsInstructionConstraintView, USDC_MINT,
};
use meteora_dlmm_commons::{
    derive_bin_array_bitmap_extension, derive_bin_array_pda, derive_event_authority_pda,
    derive_position_pda, dlmm, get_bin_array_pubkeys_for_swap, pod_read_unaligned_skip_disc,
    quote_exact_in,
};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    clock::Clock,
    commitment_config::CommitmentConfig,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    program_pack::Pack,
    pubkey::Pubkey,
};
use std::collections::HashMap;

use crate::state::{MeteoraRecord, PolicyRecord};

pub const METEORA_ADD_POLICY_SEED: u64 = 3;
pub const METEORA_REMOVE_POLICY_SEED: u64 = 4;
pub const METEORA_CLAIM_POLICY_SEED: u64 = 5;
pub const POSITION_LOWER_BIN_ID: i32 = -237;
pub const POSITION_WIDTH: i32 = 70;
pub const POSITION_UPPER_BIN_ID: i32 = POSITION_LOWER_BIN_ID + POSITION_WIDTH - 1;
pub const MAX_ACTIVE_BIN_SLIPPAGE: i32 = 3;
pub const LOYAL_ACQUIRE_USDC_RAW: u64 = 1_000;
pub const DIRECT_FEE_SWAP_USDC_RAW: u64 = 100;
pub const TEST_RANGE_A: BinRange = BinRange {
    min: -207,
    max: -199,
};
pub const TEST_RANGE_B: BinRange = BinRange {
    min: -211,
    max: -195,
};

const MAX_BIN_PER_ARRAY: i32 = 70;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct BinRange {
    pub min: i32,
    pub max: i32,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PositionSnapshot {
    pub exists: bool,
    pub lamports: u64,
    pub data_len: usize,
    pub lower_bin_id: i32,
    pub upper_bin_id: i32,
    pub nonzero_liquidity_bins: u64,
    pub pending_fee_x: u64,
    pub pending_fee_y: u64,
}

#[derive(Clone, Debug)]
pub struct MeteoraPlan {
    pub source_slot: u64,
    pub active_bin_id: i32,
    pub position: Pubkey,
    pub vault_loyal: Pubkey,
    pub vault_usdc: Pubkey,
    pub bitmap_extension_or_program_sentinel: Pubkey,
    pub bin_arrays: Vec<Pubkey>,
    pub range_a: BinRange,
    pub range_b: BinRange,
    pub policies: AutonomousMeteoraPolicies,
    pub add_constraints: Vec<SquadsInstructionConstraintView>,
    pub remove_constraints: Vec<SquadsInstructionConstraintView>,
    pub claim_constraints: Vec<SquadsInstructionConstraintView>,
}

#[derive(Clone, Debug)]
pub struct DirectFeeSwap {
    pub instruction: Instruction,
    pub amount_in_usdc_raw: u64,
    pub quoted_loyal_out_raw: u64,
    pub minimum_loyal_out_raw: u64,
    pub quoted_fee_raw: u64,
    pub quoted_protocol_fee_raw: u64,
    pub bin_arrays: Vec<Pubkey>,
}

pub fn build_direct_loyal_acquisition_swap(
    rpc: &RpcClient,
    deployment: Pubkey,
) -> Result<DirectFeeSwap> {
    build_direct_swap(rpc, deployment, LOYAL_ACQUIRE_USDC_RAW, false)
}

pub fn create_deployment_loyal_ata_instruction(deployment: Pubkey) -> Instruction {
    create_associated_token_account_idempotent_instruction(
        deployment,
        derive_associated_token_address(deployment, METEORA_LOYAL_MINT),
        deployment,
        METEORA_LOYAL_MINT,
    )
}

pub fn build_direct_fee_swap(rpc: &RpcClient, deployment: Pubkey) -> Result<DirectFeeSwap> {
    build_direct_swap(rpc, deployment, DIRECT_FEE_SWAP_USDC_RAW, true)
}

fn build_direct_swap(
    rpc: &RpcClient,
    deployment: Pubkey,
    amount_in_usdc_raw: u64,
    require_nonzero_fee: bool,
) -> Result<DirectFeeSwap> {
    let pool_account = rpc
        .get_account_with_commitment(&METEORA_POOL, CommitmentConfig::finalized())?
        .value
        .context("approved Meteora pool is absent before direct fee swap")?;
    if pool_account.owner != METEORA_DLMM_PROGRAM_ID {
        bail!("approved Meteora pool has an unexpected program owner");
    }
    require_anchor_account_discriminator(&pool_account.data, "LbPair")?;
    let pool: dlmm::accounts::LbPair =
        pod_read_unaligned_skip_disc(&pool_account.data).context("decode Meteora fee-swap pool")?;
    if pool.token_x_mint != METEORA_LOYAL_MINT
        || pool.token_y_mint != USDC_MINT
        || pool.reserve_x != METEORA_LOYAL_RESERVE
        || pool.reserve_y != METEORA_USDC_RESERVE
    {
        bail!("Meteora fee-swap pool graph does not match the approved manifest");
    }
    let (bitmap_extension_key, _) = derive_bin_array_bitmap_extension(METEORA_POOL);
    let bitmap_extension = rpc
        .get_account_with_commitment(&bitmap_extension_key, CommitmentConfig::finalized())?
        .value
        .map(|account| {
            if account.owner != METEORA_DLMM_PROGRAM_ID {
                bail!("Meteora bitmap extension has an unexpected owner");
            }
            pod_read_unaligned_skip_disc::<dlmm::accounts::BinArrayBitmapExtension>(&account.data)
                .context("decode Meteora bitmap extension")
        })
        .transpose()?;
    let bin_arrays =
        get_bin_array_pubkeys_for_swap(METEORA_POOL, &pool, bitmap_extension.as_ref(), false, 3)?;
    if bin_arrays.is_empty() {
        bail!("Meteora pool has no USDC-to-LOYAL BinArray liquidity");
    }
    let bin_accounts = rpc.get_multiple_accounts(&bin_arrays)?;
    let decoded_bin_arrays = bin_arrays
        .iter()
        .copied()
        .zip(bin_accounts)
        .map(|(key, account)| {
            let account = account.with_context(|| format!("missing fee-swap BinArray {key}"))?;
            if account.owner != METEORA_DLMM_PROGRAM_ID {
                bail!("fee-swap BinArray {key} has an unexpected owner");
            }
            let array = pod_read_unaligned_skip_disc::<dlmm::accounts::BinArray>(&account.data)
                .with_context(|| format!("decode fee-swap BinArray {key}"))?;
            Ok((key, array))
        })
        .collect::<Result<HashMap<_, _>>>()?;
    let clock_account = rpc.get_account(&solana_sdk::sysvar::clock::ID)?;
    let clock: Clock = bincode::deserialize(&clock_account.data).context("decode Clock sysvar")?;
    let mint_x = rpc.get_account(&METEORA_LOYAL_MINT)?;
    let mint_y = rpc.get_account(&USDC_MINT)?;
    let quote = quote_exact_in(
        METEORA_POOL,
        &pool,
        amount_in_usdc_raw,
        false,
        decoded_bin_arrays,
        bitmap_extension.as_ref(),
        &clock,
        &mint_x,
        &mint_y,
    )
    .context("quote direct Meteora USDC-to-LOYAL fee swap")?;
    if quote.amount_out == 0 || (require_nonzero_fee && quote.fee == 0) {
        bail!("direct Meteora dust quote does not satisfy the output/fee requirements");
    }
    let minimum_loyal_out_raw = quote
        .amount_out
        .checked_mul(9_900)
        .context("Meteora fee-swap slippage multiplication overflow")?
        / 10_000;
    if minimum_loyal_out_raw == 0 {
        bail!("direct Meteora dust quote has a zero minimum output");
    }

    let deployment_usdc = derive_associated_token_address(deployment, USDC_MINT);
    let deployment_loyal = derive_associated_token_address(deployment, METEORA_LOYAL_MINT);
    let accounts = dlmm::client::accounts::Swap2 {
        lb_pair: METEORA_POOL,
        bin_array_bitmap_extension: Some(
            bitmap_extension
                .as_ref()
                .map(|_| bitmap_extension_key)
                .unwrap_or(METEORA_DLMM_PROGRAM_ID),
        ),
        reserve_x: METEORA_LOYAL_RESERVE,
        reserve_y: METEORA_USDC_RESERVE,
        token_x_mint: METEORA_LOYAL_MINT,
        token_y_mint: USDC_MINT,
        token_x_program: spl_token::id(),
        token_y_program: spl_token::id(),
        user: deployment,
        user_token_in: deployment_usdc,
        user_token_out: deployment_loyal,
        oracle: pool.oracle,
        host_fee_in: Some(METEORA_DLMM_PROGRAM_ID),
        event_authority: METEORA_EVENT_AUTHORITY,
        program: METEORA_DLMM_PROGRAM_ID,
        memo_program: METEORA_MEMO_PROGRAM_ID,
    }
    .to_account_metas(None);
    let mut accounts = accounts.to_vec();
    accounts.extend(
        bin_arrays
            .iter()
            .copied()
            .map(|key| AccountMeta::new(key, false)),
    );
    let data = dlmm::client::args::Swap2 {
        amount_in: amount_in_usdc_raw,
        min_amount_out: minimum_loyal_out_raw,
        remaining_accounts_info: dlmm::types::RemainingAccountsInfo { slices: vec![] },
    }
    .data();
    Ok(DirectFeeSwap {
        instruction: Instruction {
            program_id: METEORA_DLMM_PROGRAM_ID,
            accounts,
            data,
        },
        amount_in_usdc_raw,
        quoted_loyal_out_raw: quote.amount_out,
        minimum_loyal_out_raw,
        quoted_fee_raw: quote.fee,
        quoted_protocol_fee_raw: quote.protocol_fee,
        bin_arrays,
    })
}

#[allow(clippy::too_many_arguments)]
pub fn load_plan(
    rpc: &RpcClient,
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
) -> Result<MeteoraPlan> {
    let response = rpc
        .get_account_with_commitment(&METEORA_POOL, CommitmentConfig::finalized())
        .context("fetch Meteora pool at finalized commitment")?;
    let pool_account = response.value.context("approved Meteora pool is absent")?;
    if pool_account.owner != METEORA_DLMM_PROGRAM_ID {
        bail!("approved Meteora pool has an unexpected program owner");
    }
    let pool: dlmm::accounts::LbPair =
        pod_read_unaligned_skip_disc(&pool_account.data).context("decode approved Meteora pool")?;
    if pool.token_x_mint != METEORA_LOYAL_MINT
        || pool.token_y_mint != USDC_MINT
        || pool.reserve_x != METEORA_LOYAL_RESERVE
        || pool.reserve_y != METEORA_USDC_RESERVE
        || pool.bin_step != 100
        || pool.status != 0
    {
        bail!("fresh Meteora pool state does not match the approved LOYAL/USDC manifest");
    }
    let (event_authority, _) = derive_event_authority_pda();
    if event_authority != METEORA_EVENT_AUTHORITY || dlmm::ID != METEORA_DLMM_PROGRAM_ID {
        bail!("official Meteora SDK identities do not match the approved manifest");
    }
    validate_token_graph(rpc)?;

    let position =
        derive_position_pda(METEORA_POOL, vault, POSITION_LOWER_BIN_ID, POSITION_WIDTH).0;
    if let Some(snapshot) = load_position_snapshot(rpc, position, vault)? {
        if snapshot.lower_bin_id != POSITION_LOWER_BIN_ID
            || snapshot.upper_bin_id != POSITION_UPPER_BIN_ID
        {
            bail!("existing Meteora position bounds do not match the approved manifest");
        }
    }

    let (bitmap_extension, _) = derive_bin_array_bitmap_extension(METEORA_POOL);
    let bitmap_extension_or_program_sentinel = match rpc
        .get_account_with_commitment(&bitmap_extension, CommitmentConfig::finalized())?
        .value
    {
        Some(account) if account.owner == METEORA_DLMM_PROGRAM_ID => bitmap_extension,
        Some(_) => bail!("Meteora bitmap extension has an unexpected owner"),
        None => METEORA_DLMM_PROGRAM_ID,
    };
    let lower_array_index = bin_array_index(POSITION_LOWER_BIN_ID);
    let upper_array_index = bin_array_index(POSITION_UPPER_BIN_ID);
    let candidate_array_indexes = [
        lower_array_index,
        upper_array_index,
        upper_array_index
            .checked_add(1)
            .context("Meteora bin-array index overflow")?,
    ];
    let bin_arrays = candidate_array_indexes
        .into_iter()
        .map(|index| derive_bin_array_pda(METEORA_POOL, i64::from(index)).0)
        .collect::<Vec<_>>();
    for (bin_array, expected_index) in bin_arrays.iter().zip(candidate_array_indexes) {
        let account = rpc
            .get_account_with_commitment(bin_array, CommitmentConfig::finalized())?
            .value
            .with_context(|| format!("required Meteora BinArray {bin_array} is absent"))?;
        if account.owner != METEORA_DLMM_PROGRAM_ID {
            bail!("required Meteora BinArray {bin_array} has an unexpected owner");
        }
        require_anchor_account_discriminator(&account.data, "BinArray")?;
        let bin_array_state: dlmm::accounts::BinArray =
            pod_read_unaligned_skip_disc(&account.data).context("decode required BinArray")?;
        if bin_array_state.lb_pair != METEORA_POOL
            || bin_array_state.index != i64::from(expected_index)
        {
            bail!("required Meteora BinArray {bin_array} pool or index is incorrect");
        }
    }

    let vault_loyal = derive_associated_token_address(vault, METEORA_LOYAL_MINT);
    let vault_usdc = derive_associated_token_address(vault, USDC_MINT);
    let policies = create_meteora_policies(
        settings,
        authority,
        delegated_signer,
        vault,
        vault_index,
        METEORA_ADD_POLICY_SEED,
        METEORA_REMOVE_POLICY_SEED,
        METEORA_CLAIM_POLICY_SEED,
        vec![position],
        vec![bin_arrays[0], bin_arrays[1]],
        vec![bin_arrays[1], bin_arrays[2]],
    )
    .context("construct split Meteora policies")?;
    let add_constraints = decoded_constraints(&policies.add_liquidity.create_instruction)?;
    let remove_constraints = decoded_constraints(&policies.remove_liquidity.create_instruction)?;
    let claim_constraints = decoded_constraints(&policies.claim_fees.create_instruction)?;
    let range_a = TEST_RANGE_A;
    let range_b = TEST_RANGE_B;
    for range in [range_a, range_b] {
        validate_position_range(range)?;
    }
    if range_a == range_b {
        bail!("Meteora verifier ranges must be distinct");
    }

    Ok(MeteoraPlan {
        source_slot: response.context.slot,
        active_bin_id: pool.active_id,
        position,
        vault_loyal,
        vault_usdc,
        bitmap_extension_or_program_sentinel,
        bin_arrays,
        range_a,
        range_b,
        policies,
        add_constraints,
        remove_constraints,
        claim_constraints,
    })
}

pub fn record_from_plan(plan: &MeteoraPlan) -> MeteoraRecord {
    MeteoraRecord {
        source_slot: plan.source_slot,
        pool: METEORA_POOL.to_string(),
        program: METEORA_DLMM_PROGRAM_ID.to_string(),
        active_bin_id_at_setup: plan.active_bin_id,
        position: plan.position.to_string(),
        position_lower_bin_id: POSITION_LOWER_BIN_ID,
        position_upper_bin_id: POSITION_UPPER_BIN_ID,
        position_width: POSITION_WIDTH,
        vault_loyal_token_account: plan.vault_loyal.to_string(),
        vault_usdc_token_account: plan.vault_usdc.to_string(),
        bitmap_extension_or_program_sentinel: plan.bitmap_extension_or_program_sentinel.to_string(),
        bin_arrays: plan.bin_arrays.iter().map(ToString::to_string).collect(),
        strategy_range_a_min_bin_id: plan.range_a.min,
        strategy_range_a_max_bin_id: plan.range_a.max,
        strategy_range_b_min_bin_id: plan.range_b.min,
        strategy_range_b_max_bin_id: plan.range_b.max,
        add_liquidity_policy: None,
        remove_liquidity_policy: None,
        claim_fee_policy: None,
        live_steps: Vec::new(),
    }
}

pub fn validate_record(record: &MeteoraRecord, plan: &MeteoraPlan) -> Result<()> {
    let fresh = record_from_plan(plan);
    if record.pool != fresh.pool
        || record.program != fresh.program
        || record.position != fresh.position
        || record.position_lower_bin_id != fresh.position_lower_bin_id
        || record.position_upper_bin_id != fresh.position_upper_bin_id
        || record.position_width != fresh.position_width
        || record.vault_loyal_token_account != fresh.vault_loyal_token_account
        || record.vault_usdc_token_account != fresh.vault_usdc_token_account
        || record.bitmap_extension_or_program_sentinel != fresh.bitmap_extension_or_program_sentinel
        || record.bin_arrays != fresh.bin_arrays
    {
        bail!("recorded Meteora account graph does not match the fresh finalized snapshot");
    }
    Ok(())
}

pub fn setup_inner_instructions(vault: Pubkey, plan: &MeteoraPlan) -> Result<Vec<Instruction>> {
    let mut instructions = vec![
        create_associated_token_account_idempotent_instruction(
            vault,
            plan.vault_loyal,
            vault,
            METEORA_LOYAL_MINT,
        ),
        create_associated_token_account_idempotent_instruction(
            vault,
            plan.vault_usdc,
            vault,
            USDC_MINT,
        ),
    ];
    let accounts = dlmm::client::accounts::InitializePositionPda {
        payer: vault,
        base: vault,
        position: plan.position,
        lb_pair: METEORA_POOL,
        owner: vault,
        system_program: solana_sdk::system_program::ID,
        rent: solana_sdk::sysvar::rent::ID,
        event_authority: METEORA_EVENT_AUTHORITY,
        program: METEORA_DLMM_PROGRAM_ID,
    }
    .to_account_metas(None);
    let data = dlmm::client::args::InitializePositionPda {
        lower_bin_id: POSITION_LOWER_BIN_ID,
        width: POSITION_WIDTH,
    }
    .data();
    instructions.push(Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts,
        data,
    });
    Ok(instructions)
}

pub fn add_liquidity_instruction(
    vault: Pubkey,
    plan: &MeteoraPlan,
    amount_loyal: u64,
    amount_usdc: u64,
    active_id: i32,
    range: BinRange,
) -> Result<Instruction> {
    validate_add_range(range, active_id)?;
    let mut accounts = dlmm::client::accounts::AddLiquidityByStrategy2 {
        position: plan.position,
        lb_pair: METEORA_POOL,
        bin_array_bitmap_extension: Some(plan.bitmap_extension_or_program_sentinel),
        user_token_x: plan.vault_loyal,
        user_token_y: plan.vault_usdc,
        reserve_x: METEORA_LOYAL_RESERVE,
        reserve_y: METEORA_USDC_RESERVE,
        token_x_mint: METEORA_LOYAL_MINT,
        token_y_mint: USDC_MINT,
        sender: vault,
        token_x_program: spl_token::id(),
        token_y_program: spl_token::id(),
        event_authority: METEORA_EVENT_AUTHORITY,
        program: METEORA_DLMM_PROGRAM_ID,
    }
    .to_account_metas(None);
    accounts.extend(bin_array_metas_for_range(range));
    let data = dlmm::client::args::AddLiquidityByStrategy2 {
        liquidity_parameter: dlmm::types::LiquidityParameterByStrategy {
            amount_x: amount_loyal,
            amount_y: amount_usdc,
            active_id,
            max_active_bin_slippage: MAX_ACTIVE_BIN_SLIPPAGE,
            strategy_parameters: dlmm::types::StrategyParameters {
                min_bin_id: range.min,
                max_bin_id: range.max,
                strategy_type: dlmm::types::StrategyType::SpotBalanced,
                parameteres: [0; 64],
            },
        },
        remaining_accounts_info: dlmm::types::RemainingAccountsInfo { slices: vec![] },
    }
    .data();
    Ok(Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts,
        data,
    })
}

pub fn remove_liquidity_instruction(
    vault: Pubkey,
    plan: &MeteoraPlan,
    range: BinRange,
    bps_to_remove: u16,
) -> Result<Instruction> {
    validate_position_range(range)?;
    if bps_to_remove == 0 || bps_to_remove > 10_000 {
        bail!("Meteora removal BPS must be between 1 and 10,000");
    }
    let mut accounts = dlmm::client::accounts::RemoveLiquidityByRange2 {
        position: plan.position,
        lb_pair: METEORA_POOL,
        bin_array_bitmap_extension: Some(plan.bitmap_extension_or_program_sentinel),
        user_token_x: plan.vault_loyal,
        user_token_y: plan.vault_usdc,
        reserve_x: METEORA_LOYAL_RESERVE,
        reserve_y: METEORA_USDC_RESERVE,
        token_x_mint: METEORA_LOYAL_MINT,
        token_y_mint: USDC_MINT,
        sender: vault,
        token_x_program: spl_token::id(),
        token_y_program: spl_token::id(),
        memo_program: METEORA_MEMO_PROGRAM_ID,
        event_authority: METEORA_EVENT_AUTHORITY,
        program: METEORA_DLMM_PROGRAM_ID,
    }
    .to_account_metas(None);
    accounts.extend(bin_array_metas_for_range(range));
    let data = dlmm::client::args::RemoveLiquidityByRange2 {
        from_bin_id: range.min,
        to_bin_id: range.max,
        bps_to_remove,
        remaining_accounts_info: dlmm::types::RemainingAccountsInfo { slices: vec![] },
    }
    .data();
    Ok(Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts,
        data,
    })
}

pub fn claim_fees_instruction(
    vault: Pubkey,
    plan: &MeteoraPlan,
    range: BinRange,
) -> Result<Instruction> {
    if range.min < POSITION_LOWER_BIN_ID
        || range.max > POSITION_UPPER_BIN_ID
        || range.min > range.max
    {
        bail!("Meteora fee-claim range is outside the approved position");
    }
    let mut accounts = dlmm::client::accounts::ClaimFee2 {
        lb_pair: METEORA_POOL,
        position: plan.position,
        sender: vault,
        reserve_x: METEORA_LOYAL_RESERVE,
        reserve_y: METEORA_USDC_RESERVE,
        user_token_x: plan.vault_loyal,
        user_token_y: plan.vault_usdc,
        token_x_mint: METEORA_LOYAL_MINT,
        token_y_mint: USDC_MINT,
        token_program_x: spl_token::id(),
        token_program_y: spl_token::id(),
        memo_program: METEORA_MEMO_PROGRAM_ID,
        event_authority: METEORA_EVENT_AUTHORITY,
        program: METEORA_DLMM_PROGRAM_ID,
    }
    .to_account_metas(None);
    accounts.extend(bin_array_metas_for_range(range));
    let data = dlmm::client::args::ClaimFee2 {
        min_bin_id: range.min,
        max_bin_id: range.max,
        remaining_accounts_info: dlmm::types::RemainingAccountsInfo { slices: vec![] },
    }
    .data();
    Ok(Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts,
        data,
    })
}

pub fn load_position_snapshot(
    rpc: &RpcClient,
    position: Pubkey,
    expected_owner: Pubkey,
) -> Result<Option<PositionSnapshot>> {
    let response = rpc.get_account_with_commitment(&position, CommitmentConfig::finalized())?;
    let Some(account) = response.value else {
        return Ok(None);
    };
    if account.owner != METEORA_DLMM_PROGRAM_ID {
        bail!("Meteora position has an unexpected program owner");
    }
    require_anchor_account_discriminator(&account.data, "PositionV2")?;
    let position_state: dlmm::accounts::PositionV2 =
        pod_read_unaligned_skip_disc(&account.data).context("decode Meteora PositionV2")?;
    if position_state.lb_pair != METEORA_POOL || position_state.owner != expected_owner {
        bail!("Meteora position pool or owner does not match the autonomous vault manifest");
    }
    let nonzero_liquidity_bins = position_state
        .liquidity_shares
        .iter()
        .filter(|shares| **shares != 0)
        .count() as u64;
    let pending_fee_x = position_state
        .fee_infos
        .iter()
        .try_fold(0_u64, |sum, fee| sum.checked_add(fee.fee_x_pending))
        .context("Meteora pending X fee overflow")?;
    let pending_fee_y = position_state
        .fee_infos
        .iter()
        .try_fold(0_u64, |sum, fee| sum.checked_add(fee.fee_y_pending))
        .context("Meteora pending Y fee overflow")?;
    Ok(Some(PositionSnapshot {
        exists: true,
        lamports: account.lamports,
        data_len: account.data.len(),
        lower_bin_id: position_state.lower_bin_id,
        upper_bin_id: position_state.upper_bin_id,
        nonzero_liquidity_bins,
        pending_fee_x,
        pending_fee_y,
    }))
}

fn validate_token_graph(rpc: &RpcClient) -> Result<()> {
    for mint in [METEORA_LOYAL_MINT, USDC_MINT] {
        let account = rpc
            .get_account_with_commitment(&mint, CommitmentConfig::finalized())?
            .value
            .with_context(|| format!("Meteora mint {mint} is absent"))?;
        if account.owner != spl_token::id() {
            bail!("Meteora mint {mint} is not owned by the classic SPL Token program");
        }
        let mint_state = spl_token::state::Mint::unpack(&account.data)
            .with_context(|| format!("decode Meteora mint {mint}"))?;
        if mint_state.decimals != 6 {
            bail!(
                "Meteora mint {mint} has {} decimals; expected 6",
                mint_state.decimals
            );
        }
    }
    for (reserve, mint) in [
        (METEORA_LOYAL_RESERVE, METEORA_LOYAL_MINT),
        (METEORA_USDC_RESERVE, USDC_MINT),
    ] {
        let account = rpc
            .get_account_with_commitment(&reserve, CommitmentConfig::finalized())?
            .value
            .with_context(|| format!("Meteora reserve {reserve} is absent"))?;
        if account.owner != spl_token::id() {
            bail!("Meteora reserve {reserve} is not a classic SPL Token account");
        }
        let token = spl_token::state::Account::unpack(&account.data)
            .with_context(|| format!("decode Meteora reserve {reserve}"))?;
        if token.mint != mint || token.owner != METEORA_POOL {
            bail!("Meteora reserve {reserve} mint or authority does not match the pool");
        }
    }
    Ok(())
}

fn require_anchor_account_discriminator(data: &[u8], account_name: &str) -> Result<()> {
    let expected = hashv(&[format!("account:{account_name}").as_bytes()]).to_bytes();
    if data.get(..8) != Some(&expected[..8]) {
        bail!("Meteora {account_name} discriminator mismatch");
    }
    Ok(())
}

fn decoded_constraints(instruction: &Instruction) -> Result<Vec<SquadsInstructionConstraintView>> {
    let actions = decode_squads_policy_create_actions(instruction)
        .context("independently decode Meteora policy-create instruction")?;
    if actions.len() != 1 {
        bail!("Meteora policy-create instruction must contain exactly one action");
    }
    Ok(actions[0].payload.constraints.clone())
}

fn validate_position_range(range: BinRange) -> Result<()> {
    if range.min < POSITION_LOWER_BIN_ID
        || range.max > POSITION_UPPER_BIN_ID
        || range.min > range.max
    {
        bail!("Meteora range is outside the approved position");
    }
    Ok(())
}

fn validate_add_range(range: BinRange, active_id: i32) -> Result<()> {
    validate_position_range(range)?;
    if range.min > active_id || range.max < active_id {
        bail!("Meteora add range does not contain the observed active bin");
    }
    Ok(())
}

fn bin_array_metas_for_range(range: BinRange) -> Vec<AccountMeta> {
    let lower = bin_array_index(range.min);
    [lower, lower + 1]
        .into_iter()
        .map(|index| {
            AccountMeta::new(
                derive_bin_array_pda(METEORA_POOL, i64::from(index)).0,
                false,
            )
        })
        .collect()
}

fn bin_array_index(bin_id: i32) -> i32 {
    bin_id.div_euclid(MAX_BIN_PER_ARRAY)
}

fn derive_associated_token_address(owner: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), spl_token::id().as_ref(), mint.as_ref()],
        &loyal_actions::ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn create_associated_token_account_idempotent_instruction(
    payer: Pubkey,
    ata: Pubkey,
    owner: Pubkey,
    mint: Pubkey,
) -> Instruction {
    Instruction {
        program_id: loyal_actions::ASSOCIATED_TOKEN_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(payer, true),
            AccountMeta::new(ata, false),
            AccountMeta::new_readonly(owner, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: vec![1],
    }
}

pub fn policy_record<'a>(
    record: &'a MeteoraRecord,
    kind: MeteoraPolicyKind,
) -> Option<&'a PolicyRecord> {
    match kind {
        MeteoraPolicyKind::AddLiquidity => record.add_liquidity_policy.as_ref(),
        MeteoraPolicyKind::RemoveLiquidity => record.remove_liquidity_policy.as_ref(),
        MeteoraPolicyKind::ClaimFees => record.claim_fee_policy.as_ref(),
    }
}

pub fn policy_record_mut(
    record: &mut MeteoraRecord,
    kind: MeteoraPolicyKind,
) -> &mut Option<PolicyRecord> {
    match kind {
        MeteoraPolicyKind::AddLiquidity => &mut record.add_liquidity_policy,
        MeteoraPolicyKind::RemoveLiquidity => &mut record.remove_liquidity_policy,
        MeteoraPolicyKind::ClaimFees => &mut record.claim_fee_policy,
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MeteoraPolicyKind {
    AddLiquidity,
    RemoveLiquidity,
    ClaimFees,
}

impl MeteoraPolicyKind {
    pub fn seed(self) -> u64 {
        match self {
            Self::AddLiquidity => METEORA_ADD_POLICY_SEED,
            Self::RemoveLiquidity => METEORA_REMOVE_POLICY_SEED,
            Self::ClaimFees => METEORA_CLAIM_POLICY_SEED,
        }
    }

    pub fn label(self) -> &'static str {
        match self {
            Self::AddLiquidity => "meteora-add-liquidity",
            Self::RemoveLiquidity => "meteora-remove-liquidity",
            Self::ClaimFees => "meteora-claim-fees",
        }
    }
}
