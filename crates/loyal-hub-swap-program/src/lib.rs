#![allow(unexpected_cfgs)]

mod codec;
mod constants;
mod instruction;
mod processor;
mod state;
mod token;
mod validation;

use pinocchio::entrypoint;

pub use constants::*;
pub use pinocchio_tkn::TOKEN_PROGRAM_ID as SPL_TOKEN_ID;
pub use processor::process_instruction;
pub use state::{derive_config, derive_hub_authority, derive_inventory_account, HubConfig};

entrypoint!(process_instruction);
