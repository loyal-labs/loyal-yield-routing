use pinocchio::program_error::ProgramError;
use pinocchio::pubkey::Pubkey;

use crate::{
    codec::{read_u16_at, read_u64_at, read_u8_at},
    constants::{
        INITIALIZE_CONFIG, MAX_REBALANCE_TRANSFERS, REBALANCE_INVENTORY, SET_MAX_FEE, SET_PAUSED,
        SWAP_EXACT_IN, WITHDRAW_INVENTORY,
    },
    state::HubConfig,
};
use loyal_hub_abi::{
    rebalance_inventory_args, rebalance_transfer, set_max_fee_args, swap_exact_in_args,
    withdraw_inventory_args,
};

pub enum HubInstruction {
    InitializeConfig(HubConfig),
    SwapExactIn(SwapExactInArgs),
    WithdrawInventory(WithdrawInventoryArgs),
    SetPaused { paused: bool },
    SetMaxFee { max_fee_bps: u16 },
    RebalanceInventory(RebalanceInventoryArgs),
}

pub struct SwapExactInArgs {
    pub amount_in: u64,
    pub amount_out: u64,
    pub min_out: u64,
    pub max_fee_bps: u16,
    pub lane_id: HubLaneId,
}

pub struct WithdrawInventoryArgs {
    pub amount: u64,
    pub lane_id: HubLaneId,
}

pub struct RebalanceInventoryArgs {
    pub transfers: Vec<RebalanceTransfer>,
}

#[derive(Clone, Copy)]
pub struct HubLaneId(pub u8);

#[derive(Clone, Copy)]
pub struct AllowedMint(pub Pubkey);

#[derive(Clone, Copy)]
pub struct InventoryAccount {
    pub lane_id: HubLaneId,
    pub mint: AllowedMint,
}

#[derive(Clone, Copy)]
pub struct RebalanceTransfer {
    pub from_lane_id: HubLaneId,
    pub to_lane_id: HubLaneId,
    pub amount: u64,
}

pub fn parse_instruction(data: &[u8]) -> Result<HubInstruction, ProgramError> {
    let (&tag, rest) = data
        .split_first()
        .ok_or(ProgramError::InvalidInstructionData)?;
    match tag {
        INITIALIZE_CONFIG => Ok(HubInstruction::InitializeConfig(HubConfig::parse(rest)?)),
        SWAP_EXACT_IN => Ok(HubInstruction::SwapExactIn(parse_swap_exact_in_args(rest)?)),
        WITHDRAW_INVENTORY => Ok(HubInstruction::WithdrawInventory(parse_withdraw_args(
            rest,
        )?)),
        SET_PAUSED => Ok(HubInstruction::SetPaused {
            paused: parse_paused(rest)?,
        }),
        SET_MAX_FEE => Ok(HubInstruction::SetMaxFee {
            max_fee_bps: parse_max_fee(rest)?,
        }),
        REBALANCE_INVENTORY => Ok(HubInstruction::RebalanceInventory(parse_rebalance_args(
            rest,
        )?)),
        _ => Err(ProgramError::InvalidInstructionData),
    }
}

fn parse_swap_exact_in_args(data: &[u8]) -> Result<SwapExactInArgs, ProgramError> {
    if data.len() != loyal_hub_abi::SWAP_EXACT_IN_ARGS_LEN {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(SwapExactInArgs {
        amount_in: read_u64_at(data, swap_exact_in_args::AMOUNT_IN_OFFSET)?,
        amount_out: read_u64_at(data, swap_exact_in_args::AMOUNT_OUT_OFFSET)?,
        min_out: read_u64_at(data, swap_exact_in_args::MIN_OUT_OFFSET)?,
        max_fee_bps: read_u16_at(data, swap_exact_in_args::MAX_FEE_BPS_OFFSET)?,
        lane_id: HubLaneId(read_u8_at(data, swap_exact_in_args::LANE_ID_OFFSET)?),
    })
}

fn parse_withdraw_args(data: &[u8]) -> Result<WithdrawInventoryArgs, ProgramError> {
    if data.len() != loyal_hub_abi::WITHDRAW_INVENTORY_ARGS_LEN {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(WithdrawInventoryArgs {
        amount: read_u64_at(data, withdraw_inventory_args::AMOUNT_OFFSET)?,
        lane_id: HubLaneId(read_u8_at(data, withdraw_inventory_args::LANE_ID_OFFSET)?),
    })
}

fn parse_paused(data: &[u8]) -> Result<bool, ProgramError> {
    if data.len() != loyal_hub_abi::SET_PAUSED_ARGS_LEN {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(read_u8_at(data, loyal_hub_abi::set_paused_args::PAUSED_OFFSET)? != 0)
}

fn parse_max_fee(data: &[u8]) -> Result<u16, ProgramError> {
    if data.len() != loyal_hub_abi::SET_MAX_FEE_ARGS_LEN {
        return Err(ProgramError::InvalidInstructionData);
    }
    read_u16_at(data, set_max_fee_args::MAX_FEE_BPS_OFFSET)
}

fn parse_rebalance_args(data: &[u8]) -> Result<RebalanceInventoryArgs, ProgramError> {
    if data.len() < rebalance_inventory_args::FIXED_LEN {
        return Err(ProgramError::InvalidInstructionData);
    }
    let transfer_count = read_u8_at(data, rebalance_inventory_args::TRANSFER_COUNT_OFFSET)?;
    let transfer_count = transfer_count as usize;
    if transfer_count == 0 || transfer_count > MAX_REBALANCE_TRANSFERS {
        return Err(ProgramError::InvalidInstructionData);
    }
    let expected_len = transfer_count
        .checked_mul(rebalance_inventory_args::TRANSFER_ITEM_LEN)
        .and_then(|len| len.checked_add(rebalance_inventory_args::FIXED_LEN))
        .ok_or(ProgramError::InvalidInstructionData)?;
    if data.len() != expected_len {
        return Err(ProgramError::InvalidInstructionData);
    }

    let mut transfers = Vec::with_capacity(transfer_count);
    for chunk in
        data[rebalance_inventory_args::TRANSFER_OFFSET..].chunks_exact(rebalance_transfer::MAX_LEN)
    {
        let from_lane_id = HubLaneId(read_u8_at(chunk, rebalance_transfer::FROM_LANE_ID_OFFSET)?);
        let to_lane_id = HubLaneId(read_u8_at(chunk, rebalance_transfer::TO_LANE_ID_OFFSET)?);
        let amount = read_u64_at(chunk, rebalance_transfer::AMOUNT_OFFSET)?;
        if amount == 0 {
            return Err(ProgramError::InvalidInstructionData);
        }
        transfers.push(RebalanceTransfer {
            from_lane_id,
            to_lane_id,
            amount,
        });
    }

    Ok(RebalanceInventoryArgs { transfers })
}
