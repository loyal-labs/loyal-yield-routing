use solana_program::program_error::ProgramError;

use crate::{
    codec::{read_u16, read_u64},
    constants::{INITIALIZE_CONFIG, SET_CONFIG, SET_PAUSED, SWAP_EXACT_IN, WITHDRAW_INVENTORY},
    state::HubConfig,
};

pub enum HubInstruction {
    InitializeConfig(HubConfig),
    SwapExactIn(SwapExactInArgs),
    WithdrawInventory { amount: u64 },
    SetPaused { paused: bool },
    SetConfig(HubConfig),
}

pub struct SwapExactInArgs {
    pub amount_in: u64,
    pub amount_out: u64,
    pub min_out: u64,
    pub max_fee_bps: u16,
}

pub fn parse_instruction(data: &[u8]) -> Result<HubInstruction, ProgramError> {
    let (&tag, rest) = data
        .split_first()
        .ok_or(ProgramError::InvalidInstructionData)?;
    match tag {
        INITIALIZE_CONFIG => Ok(HubInstruction::InitializeConfig(HubConfig::parse(rest)?)),
        SWAP_EXACT_IN => Ok(HubInstruction::SwapExactIn(parse_swap_exact_in_args(rest)?)),
        WITHDRAW_INVENTORY => Ok(HubInstruction::WithdrawInventory {
            amount: parse_withdraw_amount(rest)?,
        }),
        SET_PAUSED => Ok(HubInstruction::SetPaused {
            paused: parse_paused(rest)?,
        }),
        SET_CONFIG => Ok(HubInstruction::SetConfig(HubConfig::parse(rest)?)),
        _ => Err(ProgramError::InvalidInstructionData),
    }
}

fn parse_swap_exact_in_args(data: &[u8]) -> Result<SwapExactInArgs, ProgramError> {
    if data.len() != 26 {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(SwapExactInArgs {
        amount_in: read_u64(&data[0..8])?,
        amount_out: read_u64(&data[8..16])?,
        min_out: read_u64(&data[16..24])?,
        max_fee_bps: read_u16(&data[24..26])?,
    })
}

fn parse_withdraw_amount(data: &[u8]) -> Result<u64, ProgramError> {
    if data.len() != 8 {
        return Err(ProgramError::InvalidInstructionData);
    }
    read_u64(data)
}

fn parse_paused(data: &[u8]) -> Result<bool, ProgramError> {
    if data.len() != 1 {
        return Err(ProgramError::InvalidInstructionData);
    }
    Ok(data[0] != 0)
}
