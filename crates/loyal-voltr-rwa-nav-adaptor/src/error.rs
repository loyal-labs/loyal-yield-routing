use solana_program::program_error::ProgramError;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(u32)]
pub enum AdaptorError {
    InvalidInstruction = 0,
    InvalidAccountCount = 1,
    InvalidAccount = 2,
    InvalidConfig = 3,
    InvalidAuthority = 4,
    InvalidSquadsVault = 5,
    InvalidTokenAccount = 6,
    InvalidReport = 7,
    ReportSequence = 8,
    ReportSlot = 9,
    ReportCap = 10,
    DuplicateMutableAccount = 11,
    InsufficientBridgeLiquidity = 12,
    InvalidTicket = 13,
    InvalidTicketWritable = 14,
    TicketAlreadyArmed = 15,
    TicketNotArmed = 16,
    TicketMismatch = 17,
    TicketReplay = 18,
}

impl From<AdaptorError> for ProgramError {
    fn from(value: AdaptorError) -> Self {
        Self::Custom(value as u32)
    }
}
pub type AdaptorResult<T> = Result<T, AdaptorError>;
