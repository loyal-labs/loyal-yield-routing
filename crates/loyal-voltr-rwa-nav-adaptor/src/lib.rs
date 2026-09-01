#![allow(unexpected_cfgs)]

mod config;
mod error;
mod processor;

pub use config::*;
pub use error::*;
pub use processor::{
    process_instruction, ARM_REPORT_DISCRIMINATOR, DEPOSIT_DISCRIMINATOR,
    INITIALIZE_CONFIG_DISCRIMINATOR, INITIALIZE_DISCRIMINATOR,
    INITIALIZE_REPORT_TICKET_DISCRIMINATOR, PROGRAM_ID, REPORT_TICKET_SEED, SQUADS_PROGRAM_ID,
    WITHDRAW_DISCRIMINATOR,
};

#[cfg(not(feature = "no-entrypoint"))]
use solana_program::entrypoint;

#[cfg(not(feature = "no-entrypoint"))]
entrypoint!(process_instruction);
