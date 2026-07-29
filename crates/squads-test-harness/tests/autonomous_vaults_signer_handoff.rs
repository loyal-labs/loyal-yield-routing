use borsh::BorshDeserialize;
use loyal_actions::{
    derive_squads_v4_vault, handoff_settings_signer_instruction, SQUADS_V4_PROGRAM_ID,
};
use solana_sdk::{account::Account, pubkey::Pubkey, signer::Signer};
use solana_system_interface::instruction as system_instruction;
use squads_multisig::{
    anchor_lang::AccountSerialize,
    client::{vault_transaction_execute, VaultTransactionExecuteAccounts},
    pda::{get_multisig_pda, get_proposal_pda, get_transaction_pda, get_vault_pda},
    squads_multisig_program::VaultTransaction,
    state::{
        Member, Multisig, Permission, Permissions, Proposal, ProposalStatus, TransactionMessage,
    },
    vault_transaction::VaultTransactionMessageExt,
};
use squads_test_harness::prelude::*;

#[allow(dead_code)]
#[derive(BorshDeserialize)]
struct SettingsWire {
    discriminator: [u8; 8],
    seed: u128,
    settings_authority: Pubkey,
    threshold: u16,
    time_lock: u32,
    transaction_index: u64,
    stale_transaction_index: u64,
    archival_authority: Option<Pubkey>,
    archivable_after: u64,
    bump: u8,
    signers: Vec<SettingsSignerWire>,
    account_utilization: u8,
    policy_seed: Option<u64>,
    reserved2: u8,
}

#[derive(BorshDeserialize, Debug, PartialEq, Eq)]
struct SettingsSignerWire {
    key: Pubkey,
    permissions: SettingsPermissionsWire,
}

#[derive(BorshDeserialize, Debug, PartialEq, Eq)]
struct SettingsPermissionsWire {
    mask: u8,
}

fn anchor_account_data<T: AccountSerialize>(value: &T) -> Vec<u8> {
    let mut data = Vec::new();
    value.try_serialize(&mut data).expect("serialize account");
    data
}

fn seed_v4_account<T: AccountSerialize>(svm: &mut litesvm::LiteSVM, key: Pubkey, value: &T) {
    svm.set_account(
        key,
        Account {
            lamports: 1_000_000_000,
            data: anchor_account_data(value),
            owner: SQUADS_V4_PROGRAM_ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed Squads v4 account");
}

fn seed_v4_execution(
    svm: &mut litesvm::LiteSVM,
    multisig: Pubkey,
    member: Pubkey,
    index: u64,
    vault_index: u8,
    message: &TransactionMessage,
) -> (Pubkey, Pubkey) {
    let (transaction, transaction_bump) =
        get_transaction_pda(&multisig, index, Some(&SQUADS_V4_PROGRAM_ID));
    let (proposal, proposal_bump) = get_proposal_pda(&multisig, index, Some(&SQUADS_V4_PROGRAM_ID));
    let (_, vault_bump) = get_vault_pda(&multisig, vault_index, Some(&SQUADS_V4_PROGRAM_ID));

    seed_v4_account(
        svm,
        transaction,
        &VaultTransaction {
            multisig,
            creator: member,
            index,
            bump: transaction_bump,
            vault_index,
            vault_bump,
            ephemeral_signer_bumps: Vec::new(),
            message: message.clone().try_into().expect("compile v4 message"),
        },
    );
    seed_v4_account(
        svm,
        proposal,
        &Proposal {
            multisig,
            transaction_index: index,
            status: ProposalStatus::Approved { timestamp: 0 },
            bump: proposal_bump,
            approved: vec![member],
            rejected: Vec::new(),
            cancelled: Vec::new(),
        },
    );
    (transaction, proposal)
}

fn v4_execute_instruction(
    multisig: Pubkey,
    transaction: Pubkey,
    proposal: Pubkey,
    member: Pubkey,
    message: &TransactionMessage,
) -> solana_sdk::instruction::Instruction {
    vault_transaction_execute(
        VaultTransactionExecuteAccounts {
            multisig,
            transaction,
            member,
            proposal,
        },
        0,
        0,
        message,
        &[],
        Some(SQUADS_V4_PROGRAM_ID),
    )
    .expect("build Squads v4 execute")
}

#[test]
fn mother_v4_vault_can_control_child_settings_but_an_unrelated_vault_cannot() {
    let Some(mut context) = create_funded_squads_test_context().expect("Squads fixture") else {
        eprintln!("skipping: Squads v5 SBF fixture is unavailable");
        return;
    };
    add_squads_v4_program_from_fixture(&mut context.svm).expect("Squads v4 fixture");

    let create_key = Pubkey::new_unique();
    let (multisig, multisig_bump) = get_multisig_pda(&create_key, Some(&SQUADS_V4_PROGRAM_ID));
    let mother = get_vault_pda(&multisig, 0, Some(&SQUADS_V4_PROGRAM_ID)).0;
    let unrelated_vault = get_vault_pda(&multisig, 1, Some(&SQUADS_V4_PROGRAM_ID)).0;
    assert_eq!(derive_squads_v4_vault(&multisig, 0).0, mother);

    let member = context.wallet.pubkey();
    seed_v4_account(
        &mut context.svm,
        multisig,
        &Multisig {
            create_key,
            config_authority: Pubkey::default(),
            threshold: 1,
            time_lock: 0,
            transaction_index: 2,
            stale_transaction_index: 0,
            rent_collector: None,
            bump: multisig_bump,
            members: vec![Member {
                key: member,
                permissions: Permissions::from_vec(&[
                    Permission::Initiate,
                    Permission::Vote,
                    Permission::Execute,
                ]),
            }],
        },
    );

    let fund_mother = system_instruction::transfer(&member, &mother, 100_000_000);
    send_instructions(&mut context.svm, &[fund_mother], &context.wallet);

    let child_seed = 2u128;
    let child_settings = derive_squads_settings(child_seed).0;
    let create_child = create_squads_smart_account_instruction(member, mother, child_seed);
    send_instructions(&mut context.svm, &[create_child], &context.wallet);

    let replacement = Pubkey::new_unique();
    let wrong_handoff =
        handoff_settings_signer_instruction(child_settings, unrelated_vault, replacement)
            .expect("wrong-vault handoff shape");
    let wrong_message =
        TransactionMessage::try_compile(&mother, &[wrong_handoff], &[]).expect("compile wrong");
    let (wrong_transaction, wrong_proposal) =
        seed_v4_execution(&mut context.svm, multisig, member, 1, 0, &wrong_message);
    let mut wrong_execute = v4_execute_instruction(
        multisig,
        wrong_transaction,
        wrong_proposal,
        member,
        &wrong_message,
    );
    for account in &mut wrong_execute.accounts {
        if account.pubkey == unrelated_vault {
            account.is_signer = false;
        }
    }
    let error = try_send_instructions(&mut context.svm, &[wrong_execute], &context.wallet, &[]);
    assert!(
        error.is_err(),
        "an unrelated vault must not control child Settings"
    );

    let correct_handoff = handoff_settings_signer_instruction(child_settings, mother, replacement)
        .expect("Mother handoff");
    let correct_message = TransactionMessage::try_compile(&mother, &[correct_handoff], &[])
        .expect("compile Mother transaction");
    let (correct_transaction, correct_proposal) =
        seed_v4_execution(&mut context.svm, multisig, member, 2, 0, &correct_message);
    let correct_execute = v4_execute_instruction(
        multisig,
        correct_transaction,
        correct_proposal,
        member,
        &correct_message,
    );
    try_send_instructions(&mut context.svm, &[correct_execute], &context.wallet, &[])
        .expect("Mother v4 vault controls child Settings");

    let child_account = context
        .svm
        .get_account(&child_settings)
        .expect("child Settings persists");
    let decoded = SettingsWire::try_from_slice(&child_account.data).expect("decode child Settings");
    assert_eq!(decoded.threshold, 1);
    assert_eq!(decoded.time_lock, 0);
    assert_eq!(
        decoded.signers,
        vec![SettingsSignerWire {
            key: replacement,
            permissions: SettingsPermissionsWire { mask: 7 },
        }]
    );
}
