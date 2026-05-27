use solana_program::program_error::ProgramError;

use crate::{
    codec::{read_u16, read_u64},
    constants::{
        INITIALIZE_CONFIG, MAX_REBALANCE_TRANSFERS, REBALANCE_INVENTORY, SET_MAX_FEE, SET_PAUSED,
        SWAP_EXACT_IN, WITHDRAW_INVENTORY,
    },
    state::HubConfig,
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
    pub lane_id: u8,
}

pub struct WithdrawInventoryArgs {
    pub amount: u64,
    pub lane_id: u8,
}

pub struct RebalanceInventoryArgs {
    pub transfers: Vec<RebalanceTransfer>,
}

#[derive(Clone, Copy)]
pub struct RebalanceTransfer {
    pub from_lane_id: u8,
    pub to_lane_id: u8,
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
    if data.len() != 27 {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(SwapExactInArgs {
        amount_in: read_u64(&data[0..8])?,
        amount_out: read_u64(&data[8..16])?,
        min_out: read_u64(&data[16..24])?,
        max_fee_bps: read_u16(&data[24..26])?,
        lane_id: data[26],
    })
}

fn parse_withdraw_args(data: &[u8]) -> Result<WithdrawInventoryArgs, ProgramError> {
    if data.len() != 9 {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(WithdrawInventoryArgs {
        amount: read_u64(&data[0..8])?,
        lane_id: data[8],
    })
}

fn parse_paused(data: &[u8]) -> Result<bool, ProgramError> {
    if data.len() != 1 {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(data[0] != 0)
}

fn parse_max_fee(data: &[u8]) -> Result<u16, ProgramError> {
    if data.len() != 2 {
        return Err(ProgramError::InvalidInstructionData);
    }
    read_u16(data)
}

fn parse_rebalance_args(data: &[u8]) -> Result<RebalanceInventoryArgs, ProgramError> {
    let (&transfer_count, rest) = data
        .split_first()
        .ok_or(ProgramError::InvalidInstructionData)?;
    let transfer_count = transfer_count as usize;
    if transfer_count == 0 || transfer_count > MAX_REBALANCE_TRANSFERS {
        return Err(ProgramError::InvalidInstructionData);
    }
    if rest.len() != transfer_count * 10 {
        return Err(ProgramError::InvalidInstructionData);
    }

    let mut transfers = Vec::with_capacity(transfer_count);
    for index in 0..transfer_count {
        let offset = index * 10;
        let from_lane_id = rest[offset];
        let to_lane_id = rest[offset + 1];
        let amount = read_u64(&rest[offset + 2..offset + 10])?;
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
