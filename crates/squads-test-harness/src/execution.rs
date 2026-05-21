#![allow(dead_code, unused_imports)]

use borsh::BorshSerialize;
use litesvm::LiteSVM;
use solana_sdk::{
    account::Account,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    message::Message,
    pubkey,
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use spl_token::solana_program::{program_option::COption, program_pack::Pack};
use std::{env, fs, io::Write, path::PathBuf};

use crate::types::*;
use crate::*;

pub fn serialize_squads_sync_transaction_args(account_index: u8, payload: Vec<u8>) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    account_index.serialize(&mut data).unwrap();
    SQUADS_SYNC_SIGNER_COUNT.serialize(&mut data).unwrap();
    0u8.serialize(&mut data).unwrap();
    payload.serialize(&mut data).unwrap();
    data
}

pub(crate) fn serialize_squads_sync_settings_transaction_args(
    actions: Vec<SquadsSettingsAction>,
) -> Vec<u8> {
    let mut data = Vec::from(anchor_instruction_discriminator(
        "execute_settings_transaction_sync",
    ));
    SquadsSyncSettingsTransactionArgs {
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        actions,
        memo: None,
    }
    .serialize(&mut data)
    .unwrap();
    data
}

pub(crate) fn serialize_squads_sync_policy_payload_args(
    account_index: u8,
    policy_payload: SquadsPolicyPayload,
) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Policy(policy_payload),
    }
    .serialize(&mut data)
    .unwrap();
    data
}

pub fn execute_squads_sync_transfer_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    recipient: Pubkey,
    lamports: u64,
) -> Instruction {
    let (vault, _) = derive_squads_vault(&squads_settings, account_index);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(vault, false),
            AccountMeta::new(recipient, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: serialize_squads_sync_transaction_args(
            account_index,
            squads_system_transfer_payload(lamports),
        ),
    }
}

pub fn execute_squads_sync_transaction_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(squads_settings, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_transaction_args(
            account_index,
            squads_compiled_instruction_payload(&compiled_instructions),
        ),
    }
}

pub fn execute_mock_jupiter_sol_to_usdc_swap_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_usdc_token_account: Pubkey,
    jupiter_sol_escrow: Pubkey,
    amount: u64,
) -> Instruction {
    let jupiter_accounts = mock_jupiter_token_accounts();
    execute_squads_sync_transaction_instruction(
        squads_settings,
        signer,
        account_index,
        vec![
            SquadsCompiledInstruction {
                program_id_index: 2,
                accounts: vec![0, 1],
                data: system_transfer_data(amount),
            },
            SquadsCompiledInstruction {
                program_id_index: 3,
                accounts: vec![0, 4, 5, 6, 7, 8],
                data: mock_jupiter_swap_data(
                    MOCK_JUPITER_SOL_TO_USDC,
                    amount,
                    WRAPPED_SOL_MINT,
                    USDC_MINT,
                ),
            },
        ],
        vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(jupiter_sol_escrow, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
            AccountMeta::new(vault_usdc_token_account, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new(jupiter_accounts.usdc_reserve, false),
            AccountMeta::new_readonly(jupiter_accounts.authority, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
    )
}

pub fn execute_squads_yield_route_stable_swap_instruction(
    swap_policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    in_amount: u64,
    out_amount: u64,
) -> Instruction {
    execute_squads_program_interaction_instruction(
        swap_policy,
        signer,
        account_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 9,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8],
            data: mock_jupiter_stable_exact_in_swap_data(
                in_amount,
                out_amount,
                input_mint,
                output_mint,
            ),
        }],
        vec![0],
        vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(vault_input, false),
            AccountMeta::new(vault_output, false),
            AccountMeta::new_readonly(input_mint, false),
            AccountMeta::new_readonly(output_mint, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new(mock_jupiter_stable_reserve_token_account(input_mint), false),
            AccountMeta::new(
                mock_jupiter_stable_reserve_token_account(output_mint),
                false,
            ),
            AccountMeta::new_readonly(derive_mock_jupiter_swap_authority(), false),
            AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
        ],
    )
}

pub fn initialize_loyal_hub_config_instruction(
    payer: Pubkey,
    admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Instruction {
    assert_eq!(
        payer, admin,
        "test helper initializes config with the admin as payer"
    );
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(payer, true),
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: loyal_hub_initialize_config_data(
            admin,
            hub_authorizer,
            max_fee_bps,
            paused,
            allowed_mints,
        ),
    }
}

pub fn set_loyal_hub_paused_instruction(admin: Pubkey, paused: bool) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(admin, true),
        ],
        data: loyal_hub_set_paused_data(paused),
    }
}

pub fn set_loyal_hub_config_instruction(
    admin_signer: Pubkey,
    new_admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    allowed_mints: &[Pubkey],
) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(admin_signer, true),
        ],
        data: loyal_hub_set_config_data(
            new_admin,
            hub_authorizer,
            max_fee_bps,
            paused,
            allowed_mints,
        ),
    }
}

pub fn withdraw_loyal_hub_inventory_instruction(
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
) -> Instruction {
    Instruction {
        program_id: LOYAL_HUB_SWAP_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(derive_loyal_hub_config(), false),
            AccountMeta::new_readonly(admin, true),
            AccountMeta::new(hub_source, false),
            AccountMeta::new(destination, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: loyal_hub_withdraw_inventory_data(amount),
    }
}

pub fn execute_squads_yield_route_loyal_hub_swap_instruction(
    swap_policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
) -> Instruction {
    execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        swap_policy,
        signer,
        account_index,
        vault,
        vault_input,
        vault_output,
        input_mint,
        output_mint,
        hub_authorizer,
        amount_in,
        amount_out,
        min_out,
        max_fee_bps,
        0,
    )
}

pub fn execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
    swap_policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    hub_authorizer: Pubkey,
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
    instruction_constraint_index: u8,
) -> Instruction {
    execute_squads_program_interaction_instruction(
        swap_policy,
        signer,
        account_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 11,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
            data: loyal_hub_swap_exact_in_data(amount_in, amount_out, min_out, max_fee_bps),
        }],
        vec![instruction_constraint_index],
        vec![
            AccountMeta::new_readonly(derive_loyal_hub_config(), false),
            AccountMeta::new(vault, false),
            AccountMeta::new(vault_input, false),
            AccountMeta::new(vault_output, false),
            AccountMeta::new(loyal_hub_token_account(input_mint), false),
            AccountMeta::new(loyal_hub_token_account(output_mint), false),
            AccountMeta::new_readonly(input_mint, false),
            AccountMeta::new_readonly(output_mint, false),
            AccountMeta::new_readonly(derive_loyal_hub_authority(), false),
            AccountMeta::new_readonly(hub_authorizer, true),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
        ],
    )
}
