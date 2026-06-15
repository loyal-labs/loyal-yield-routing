#![allow(unexpected_cfgs)]
#![cfg_attr(kani, allow(dead_code, unreachable_code, unused_imports))]

mod codec;
mod constants;
mod instruction;
mod processor;
mod state;
mod token;
mod validation;

use pinocchio::entrypoint;

pub use constants::*;
pub use pinocchio_tkn::{
    TOKEN_2022_PROGRAM_ID as SPL_TOKEN_2022_ID, TOKEN_PROGRAM_ID as SPL_TOKEN_ID,
};
pub use processor::process_instruction;
pub use state::{
    derive_config, derive_hub_authority, derive_inventory_account,
    derive_inventory_account_for_token_program, HubConfig,
};

#[cfg(kani)]
extern crate kani;
#[cfg(kani)]
mod kani_contracts;
#[cfg(kani)]
mod kani_impl;

entrypoint!(process_instruction);
