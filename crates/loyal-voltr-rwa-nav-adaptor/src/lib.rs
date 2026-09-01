#![allow(unexpected_cfgs)]

mod config;
mod error;
mod processor;

pub use config::*;
pub use error::*;
pub use processor::{
    process_instruction, DEPOSIT_DISCRIMINATOR, INITIALIZE_CONFIG_DISCRIMINATOR,
    INITIALIZE_DISCRIMINATOR, PROGRAM_ID, SQUADS_PROGRAM_ID, WITHDRAW_DISCRIMINATOR,
};

#[cfg(not(feature = "no-entrypoint"))]
use solana_program::entrypoint;

#[cfg(not(feature = "no-entrypoint"))]
entrypoint!(process_instruction);
