#![allow(unexpected_cfgs)]

mod codec;
mod constants;
mod instruction;
mod processor;
mod state;
mod token;
mod validation;

use solana_program::{
    account_info::AccountInfo, entrypoint, entrypoint::ProgramResult, pubkey::Pubkey,
};

pub use constants::*;
pub use processor::process_instruction;
pub use state::{derive_config, derive_hub_authority, derive_inventory_account, HubConfig};

entrypoint!(entrypoint_process_instruction);

fn entrypoint_process_instruction(
    program_id: &Pubkey,
    accounts: &[AccountInfo],
    data: &[u8],
) -> ProgramResult {
    processor::process_instruction(program_id, accounts, data)
}
