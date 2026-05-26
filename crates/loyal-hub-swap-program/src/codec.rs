use solana_program::program_error::ProgramError;

pub fn read_u16(data: &[u8]) -> Result<u16, ProgramError> {
    Ok(u16::from_le_bytes(
        data.try_into()
            .map_err(|_| ProgramError::InvalidInstructionData)?,
    ))
}

pub fn read_u64(data: &[u8]) -> Result<u64, ProgramError> {
    Ok(u64::from_le_bytes(
        data.try_into()
            .map_err(|_| ProgramError::InvalidInstructionData)?,
    ))
}

pub fn read_pubkey(data: &[u8]) -> Result<[u8; 32], ProgramError> {
    data.try_into()
        .map_err(|_| ProgramError::InvalidInstructionData)
}
