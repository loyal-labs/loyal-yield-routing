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

pub fn execute_squads_spending_limit_withdrawal_instruction(
    policy: Pubkey,
    signer: Pubkey,
    squads_settings: Pubkey,
    source_account_index: u8,
    destination: Pubkey,
    lamports: u64,
) -> Instruction {
    let (vault, _) = derive_squads_vault(&squads_settings, source_account_index);
    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(policy, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(vault, false),
            AccountMeta::new(destination, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: serialize_squads_sync_policy_payload_args(
            source_account_index,
            SquadsPolicyPayload::SpendingLimit(SquadsSpendingLimitPayload {
                amount: lamports,
                destination,
                decimals: SOL_DECIMALS,
            }),
        ),
    }
}

pub fn execute_squads_program_interaction_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Instruction {
    let mut accounts = vec![
        AccountMeta::new(policy, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_policy_payload_args(
            account_index,
            SquadsPolicyPayload::ProgramInteraction(SquadsProgramInteractionPayload {
                instruction_constraint_indices: Some(instruction_constraint_indices),
                transaction_payload: SquadsProgramInteractionTransactionPayload::SyncTransaction(
                    SquadsProgramInteractionSyncPayload {
                        account_index,
                        instructions: squads_compiled_instruction_payload(&compiled_instructions),
                    },
                ),
            }),
        ),
    }
}

pub fn hash_squads_account_metas(accounts: &[AccountMeta]) -> [u8; 32] {
    let mut bytes = Vec::with_capacity(accounts.len() * 34);
    for account in accounts {
        bytes.extend_from_slice(account.pubkey.as_ref());
        bytes.push(u8::from(account.is_writable));
        bytes.push(u8::from(account.is_signer));
    }

    hashv(&[&bytes]).to_bytes()
}

pub fn add_squads_program_from_env(svm: &mut LiteSVM) -> std::io::Result<Option<PathBuf>> {
    let Some(path) = env::var_os("SQUADS_SMART_ACCOUNT_PROGRAM_SO").map(PathBuf::from) else {
        return Ok(None);
    };
    let program = fs::read(&path)?;
    svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
        .map_err(|error| std::io::Error::other(format!("add Squads program failed: {error}")))?;
    Ok(Some(path))
}

pub fn add_squads_program_from_env_or_sibling_checkout(
    svm: &mut LiteSVM,
) -> std::io::Result<Option<PathBuf>> {
    if let Some(path) = add_squads_program_from_env(svm)? {
        return Ok(Some(path));
    }

    let fixture_path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .join(SQUADS_SMART_ACCOUNT_PROGRAM_SO_FIXTURE);
    if fixture_path.exists() {
        let program = fs::read(&fixture_path)?;
        svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
            .map_err(|error| {
                std::io::Error::other(format!("add Squads program failed: {error}"))
            })?;
        return Ok(Some(fixture_path));
    }

    let sibling_path =
        PathBuf::from("../passkey-work/target/deploy/squads_smart_account_program.so");
    if !sibling_path.exists() {
        return Ok(None);
    }

    let program = fs::read(&sibling_path)?;
    svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
        .map_err(|error| std::io::Error::other(format!("add Squads program failed: {error}")))?;
    Ok(Some(sibling_path))
}

pub fn create_funded_squads_test_context() -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config(FundedSquadsTestConfig::default())
}

pub fn create_funded_squads_test_context_with_mock_programs(
    mock_programs: &[MockProgram],
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config_and_mock_programs(
        FundedSquadsTestConfig::default(),
        mock_programs,
    )
}

pub fn create_funded_squads_test_context_with_config(
    config: FundedSquadsTestConfig,
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config_and_mock_programs(config, &[])
}

pub fn create_funded_squads_test_context_with_config_and_mock_programs(
    config: FundedSquadsTestConfig,
    mock_programs: &[MockProgram],
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    assert!(
        config.vault_funding_lamports < config.wallet_airdrop_lamports,
        "vault funding should leave the wallet funded for later operations"
    );

    let mut svm = new_litesvm();
    let Some(loaded_program_path) = add_squads_program_from_env_or_sibling_checkout(&mut svm)?
    else {
        return Ok(None);
    };
    for mock_program in mock_programs {
        match mock_program {
            MockProgram::Jupiter => {
                add_mock_jupiter_program(&mut svm)?;
            }
            MockProgram::KaminoLend => {
                add_mock_kamino_lend_program(&mut svm)?;
            }
            MockProgram::LoyalHubSwap => {
                add_loyal_hub_swap_program(&mut svm)?;
            }
        }
    }

    let wallet = Keypair::new();
    svm.airdrop(&wallet.pubkey(), config.wallet_airdrop_lamports)
        .expect("airdrop test wallet");
    seed_squads_program_config(&mut svm, wallet.pubkey(), squads_test_treasury(), 0);

    let pool = derive_squads_pool(config.smart_account_seed);
    let create_smart_account_ix = create_squads_smart_account_instruction(
        wallet.pubkey(),
        wallet.pubkey(),
        config.smart_account_seed,
    );
    send_instructions(&mut svm, &[create_smart_account_ix], &wallet);

    let settings_account = svm
        .get_account(&pool.settings)
        .expect("Squads settings account created");
    assert_eq!(settings_account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);

    let (vault, _) = derive_squads_vault(&pool.settings, config.vault_index);
    let fund_vault_ix =
        system_instruction::transfer(&wallet.pubkey(), &vault, config.vault_funding_lamports);
    send_instructions(&mut svm, &[fund_vault_ix], &wallet);

    let context = FundedSquadsTestContext {
        svm,
        wallet,
        pool,
        vault_index: config.vault_index,
        vault,
        wallet_airdrop_lamports: config.wallet_airdrop_lamports,
        vault_funding_lamports: config.vault_funding_lamports,
        loaded_program_path,
    };
    assert_eq!(context.vault_balance(), config.vault_funding_lamports);
    assert!(context.wallet_balance() > 0);

    Ok(Some(context))
}

pub fn send_instructions(svm: &mut LiteSVM, instructions: &[Instruction], payer: &Keypair) {
    try_send_instructions(svm, instructions, payer, &[]).unwrap();
}

pub fn try_send_instructions(
    svm: &mut LiteSVM,
    instructions: &[Instruction],
    payer: &Keypair,
    additional_signers: &[&Keypair],
) -> Result<(), String> {
    svm.expire_blockhash();
    let message =
        Message::new_with_blockhash(instructions, Some(&payer.pubkey()), &svm.latest_blockhash());
    let mut signers = Vec::with_capacity(additional_signers.len() + 1);
    signers.push(payer);
    signers.extend_from_slice(additional_signers);
    let transaction = Transaction::new(&signers, message, svm.latest_blockhash());
    svm.send_transaction(transaction)
        .map(|_| ())
        .map_err(|error| format!("{error:?}"))
}

pub fn hash32(value: &[u8]) -> [u8; 32] {
    hashv(&[value]).to_bytes()
}
