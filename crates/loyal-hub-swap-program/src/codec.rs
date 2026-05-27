use pinocchio::program_error::ProgramError;

fn read_fixed<const N: usize>(data: &[u8], offset: usize) -> Result<[u8; N], ProgramError> {
    let end = offset
        .checked_add(N)
        .ok_or(ProgramError::InvalidInstructionData)?;
    data.get(offset..end)
        .ok_or(ProgramError::InvalidInstructionData)?
        .try_into()
        .map_err(|_| ProgramError::InvalidInstructionData)
}

pub fn read_u8_at(data: &[u8], offset: usize) -> Result<u8, ProgramError> {
    data.get(offset)
        .copied()
        .ok_or(ProgramError::InvalidInstructionData)
}

pub fn read_u16_at(data: &[u8], offset: usize) -> Result<u16, ProgramError> {
    Ok(u16::from_le_bytes(read_fixed(data, offset)?))
}

pub fn read_u64_at(data: &[u8], offset: usize) -> Result<u64, ProgramError> {
    Ok(u64::from_le_bytes(read_fixed(data, offset)?))
}

pub fn read_pubkey_at(data: &[u8], offset: usize) -> Result<[u8; 32], ProgramError> {
    read_fixed(data, offset)
}

pub fn write_bytes_at(data: &mut [u8], offset: usize, bytes: &[u8]) -> Result<(), ProgramError> {
    let end = offset
        .checked_add(bytes.len())
        .ok_or(ProgramError::InvalidInstructionData)?;
    data.get_mut(offset..end)
        .ok_or(ProgramError::InvalidInstructionData)?
        .copy_from_slice(bytes);
    Ok(())
}
