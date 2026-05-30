use borsh::BorshSerialize;
use litesvm::LiteSVM;
use loyal_actions::LoyalActionStep;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};
use spl_token::solana_program::program_pack::Pack;

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

pub fn execute_squads_internal_fund_transfer_instruction(
    policy: Pubkey,
    signer: Pubkey,
    squads_settings: Pubkey,
    source_index: u8,
    destination_index: u8,
    source_token_account: Pubkey,
    destination_token_account: Pubkey,
    mint: Pubkey,
    decimals: u8,
    amount: u64,
) -> Instruction {
    let (source_account, _) = derive_squads_vault(&squads_settings, source_index);
    execute_squads_internal_fund_transfer_instruction_with_accounts(
        policy,
        signer,
        source_index,
        SquadsInternalFundTransferPayload {
            source_index,
            destination_index,
            mint,
            decimals,
            amount,
        },
        vec![
            AccountMeta::new_readonly(source_account, false),
            AccountMeta::new(source_token_account, false),
            AccountMeta::new(destination_token_account, false),
            AccountMeta::new_readonly(mint, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
    )
}

pub fn execute_squads_internal_fund_transfer_instruction_from_mint_account(
    svm: &LiteSVM,
    policy: Pubkey,
    signer: Pubkey,
    squads_settings: Pubkey,
    source_index: u8,
    destination_index: u8,
    source_token_account: Pubkey,
    destination_token_account: Pubkey,
    mint: Pubkey,
    amount: u64,
) -> Instruction {
    let mint_account = svm.get_account(&mint).expect("SPL mint account exists");
    let mint_state =
        spl_token::state::Mint::unpack(&mint_account.data).expect("unpack SPL mint account");
    execute_squads_internal_fund_transfer_instruction(
        policy,
        signer,
        squads_settings,
        source_index,
        destination_index,
        source_token_account,
        destination_token_account,
        mint,
        mint_state.decimals,
        amount,
    )
}

pub(crate) fn execute_squads_internal_fund_transfer_instruction_with_accounts(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    payload: SquadsInternalFundTransferPayload,
    mut transfer_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(policy, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transfer_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_policy_payload_args(
            account_index,
            SquadsPolicyPayload::InternalFundTransfer(payload),
        ),
    }
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

pub fn execute_loyal_action_step(
    step: LoyalActionStep,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    execute_squads_program_interaction_instruction(
        step.action_account(),
        signer,
        account_index,
        compiled_instructions,
        step.instruction_constraint_indexes(),
        transaction_accounts,
    )
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
    execute_squads_yield_route_stable_swap_instruction_with_constraint_index(
        swap_policy,
        signer,
        account_index,
        vault,
        vault_input,
        vault_output,
        input_mint,
        output_mint,
        in_amount,
        out_amount,
        0,
    )
}

pub fn execute_squads_yield_route_stable_swap_instruction_with_constraint_index(
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
    instruction_constraint_index: u8,
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
        vec![instruction_constraint_index],
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

pub fn execute_loyal_action_jupiter_swap(
    step: LoyalActionStep,
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
    execute_squads_yield_route_stable_swap_instruction_with_constraint_index(
        step.action_account(),
        signer,
        account_index,
        vault,
        vault_input,
        vault_output,
        input_mint,
        output_mint,
        in_amount,
        out_amount,
        step.instruction_constraint_index(),
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
    initialize_loyal_hub_config_instruction_with_lane_count(
        payer,
        admin,
        hub_authorizer,
        max_fee_bps,
        paused,
        DEFAULT_LOYAL_HUB_LANE_COUNT,
        allowed_mints,
    )
}

pub fn initialize_loyal_hub_config_instruction_with_lane_count(
    payer: Pubkey,
    admin: Pubkey,
    hub_authorizer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Instruction {
    initialize_loyal_hub_config_instruction_with_rebalancer_and_lane_count(
        payer,
        admin,
        hub_authorizer,
        hub_authorizer,
        max_fee_bps,
        paused,
        lane_count,
        allowed_mints,
    )
}

pub fn initialize_loyal_hub_config_instruction_with_rebalancer_and_lane_count(
    payer: Pubkey,
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Instruction {
    assert_eq!(
        payer, admin,
        "test helper initializes config with the admin as payer"
    );
    loyal_actions::loyal_hub_initialize_config_instruction(
        payer,
        admin,
        hub_authorizer,
        inventory_rebalancer,
        max_fee_bps,
        paused,
        lane_count,
        allowed_mints,
    )
    .expect("valid Loyal Hub initialize config instruction")
}

pub fn rebalance_loyal_hub_inventory_instruction(
    inventory_rebalancer: Pubkey,
    mint: Pubkey,
    transfers: &[LoyalHubRebalanceTransfer],
) -> Instruction {
    loyal_actions::loyal_hub_rebalance_inventory_instruction(inventory_rebalancer, mint, transfers)
        .expect("valid Loyal Hub rebalance instruction")
}

pub fn execute_squads_loyal_hub_rebalance_batch_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    vault_index: u8,
    inventory_rebalancer: Pubkey,
    transfer_groups: &[(Pubkey, Vec<LoyalHubRebalanceTransfer>)],
) -> Instruction {
    assert!(
        !transfer_groups.is_empty(),
        "Loyal Hub owner rebalance needs at least one mint group"
    );

    let mut transaction_accounts = Vec::new();
    let mut compiled_instructions = Vec::with_capacity(transfer_groups.len());
    for (mint, transfers) in transfer_groups {
        let ix = loyal_actions::loyal_hub_rebalance_inventory_instruction(
            inventory_rebalancer,
            *mint,
            transfers,
        )
        .expect("valid Loyal Hub rebalance instruction");
        let accounts = ix
            .accounts
            .into_iter()
            .enumerate()
            .map(|(index, mut account)| {
                if index == 1 {
                    account.is_signer = false;
                }
                push_or_update_account_meta(&mut transaction_accounts, account)
            })
            .collect();
        let program_id_index = push_or_update_account_meta(
            &mut transaction_accounts,
            AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
        );
        compiled_instructions.push(SquadsCompiledInstruction {
            program_id_index,
            accounts,
            data: ix.data,
        });
    }

    execute_squads_sync_transaction_instruction(
        squads_settings,
        signer,
        vault_index,
        compiled_instructions,
        transaction_accounts,
    )
}

pub(crate) fn push_or_update_account_meta(
    accounts: &mut Vec<AccountMeta>,
    meta: AccountMeta,
) -> usize {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_writable |= meta.is_writable;
        accounts[index].is_signer |= meta.is_signer;
        return index;
    }

    let index = accounts.len();
    accounts.push(meta);
    index
}

pub(crate) fn merge_compiled_instructions(
    transaction_accounts: &mut Vec<AccountMeta>,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    source_accounts: Vec<AccountMeta>,
) -> Vec<SquadsCompiledInstruction> {
    compiled_instructions
        .into_iter()
        .map(|instruction| {
            let program_id_index = remap_account_index(
                transaction_accounts,
                &source_accounts,
                instruction.program_id_index,
            );
            let accounts = instruction
                .accounts
                .into_iter()
                .map(|index| remap_account_index(transaction_accounts, &source_accounts, index))
                .collect();
            SquadsCompiledInstruction {
                program_id_index,
                accounts,
                data: instruction.data,
            }
        })
        .collect()
}

pub(crate) fn compile_inner_instruction(
    transaction_accounts: &mut Vec<AccountMeta>,
    instruction: Instruction,
) -> SquadsCompiledInstruction {
    let accounts = instruction
        .accounts
        .into_iter()
        .map(|account| push_or_update_account_meta(transaction_accounts, account))
        .collect();
    let program_id_index = push_or_update_account_meta(
        transaction_accounts,
        AccountMeta::new_readonly(instruction.program_id, false),
    );

    SquadsCompiledInstruction {
        program_id_index,
        accounts,
        data: instruction.data,
    }
}

fn remap_account_index(
    transaction_accounts: &mut Vec<AccountMeta>,
    source_accounts: &[AccountMeta],
    index: usize,
) -> usize {
    let account = source_accounts
        .get(index)
        .unwrap_or_else(|| panic!("compiled instruction account index {index} is out of bounds"));
    push_or_update_account_meta(transaction_accounts, account.clone())
}

pub fn set_loyal_hub_paused_instruction(admin: Pubkey, paused: bool) -> Instruction {
    loyal_actions::loyal_hub_set_paused_instruction(admin, paused)
}

pub fn set_loyal_hub_max_fee_instruction(admin: Pubkey, max_fee_bps: u16) -> Instruction {
    loyal_actions::loyal_hub_set_max_fee_instruction(admin, max_fee_bps)
        .expect("valid Loyal Hub set max fee instruction")
}

pub fn withdraw_loyal_hub_inventory_instruction(
    admin: Pubkey,
    hub_source: Pubkey,
    destination: Pubkey,
    mint: Pubkey,
    amount: u64,
    lane_id: u8,
) -> Instruction {
    loyal_actions::loyal_hub_withdraw_inventory_instruction_with_source(
        admin,
        hub_source,
        destination,
        mint,
        amount,
        lane_id,
    )
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
    lane_id: u8,
    instruction_constraint_index: u8,
) -> Instruction {
    let hub_swap_ix = loyal_actions::loyal_hub_swap_exact_in_instruction(
        vault,
        vault_input,
        vault_output,
        input_mint,
        output_mint,
        hub_authorizer,
        loyal_actions::LoyalHubSwapExactIn {
            amount_in,
            amount_out,
            min_out,
            max_fee_bps,
            lane_id,
        },
    );
    let mut transaction_accounts = hub_swap_ix.accounts;
    transaction_accounts[1].is_signer = false;
    transaction_accounts.push(AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false));

    execute_squads_program_interaction_instruction(
        swap_policy,
        signer,
        account_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 11,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
            data: hub_swap_ix.data,
        }],
        vec![instruction_constraint_index],
        transaction_accounts,
    )
}

pub fn execute_loyal_action_hub_swap(
    step: LoyalActionStep,
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
    lane_id: u8,
) -> Instruction {
    execute_squads_yield_route_loyal_hub_swap_instruction_with_constraint_index(
        step.action_account(),
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
        lane_id,
        step.instruction_constraint_index(),
    )
}
