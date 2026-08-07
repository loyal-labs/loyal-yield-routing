use borsh::BorshSerialize;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
};
use solana_system_interface::instruction as system_instruction;
use squads_test_harness::{
    create_first_squads_internal_fund_transfer_policy_instruction,
    create_funded_squads_test_context, derive_squads_vault,
    execute_squads_internal_fund_transfer_instruction,
    execute_squads_internal_fund_transfer_instruction_from_mint_account, get_spl_token_amount,
    remove_squads_policy_instruction, seed_spl_mint_if_missing, seed_spl_token_account,
    supported_internal_fund_transfer_stable_mints, try_send_instructions,
    SquadsInternalFundTransferPayload, LAMPORTS_PER_SOL,
    SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    SQUADS_SYNC_SIGNER_COUNT, USDC_DECIMALS, USDC_MINT,
};

const USDE_DECIMALS: u8 = 9;

struct InternalFundTransferFixture {
    context: squads_test_harness::FundedSquadsTestContext,
    wallet_b: Keypair,
    policy: Pubkey,
    vault_0: Pubkey,
    vault_1: Pubkey,
    usdc_source_0: Pubkey,
    usdc_source_1: Pubkey,
    usde_source_0: Pubkey,
    usde_source_1: Pubkey,
}

#[derive(BorshSerialize)]
enum TestSquadsPolicyPayload {
    InternalFundTransfer(SquadsInternalFundTransferPayload),
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum TestSquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(TestSquadsPolicyPayload),
}

#[derive(BorshSerialize)]
struct TestSquadsSyncTransactionArgs {
    account_index: u8,
    num_signers: u8,
    payload: TestSquadsSyncPayload,
}

fn execute_squads_internal_fund_transfer_instruction_with_accounts(
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

    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    TestSquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: TestSquadsSyncPayload::Policy(TestSquadsPolicyPayload::InternalFundTransfer(
            payload,
        )),
    }
    .serialize(&mut data)
    .unwrap();

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data,
    }
}

impl InternalFundTransferFixture {
    fn new() -> Option<Self> {
        let mut context =
            create_funded_squads_test_context().expect("create funded Squads test context")?;
        let wallet_b = Keypair::new();
        context
            .svm
            .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
            .expect("airdrop wallet B");

        let (vault_0, _) = derive_squads_vault(&context.pool.settings, 0);
        let (vault_1, _) = derive_squads_vault(&context.pool.settings, 1);
        let fund_vault_1 =
            system_instruction::transfer(&context.wallet_pubkey(), &vault_1, LAMPORTS_PER_SOL / 4);
        try_send_instructions(&mut context.svm, &[fund_vault_1], &context.wallet, &[])
            .expect("fund Squads subaccount 1");

        seed_spl_mint_if_missing(&mut context.svm, USDC_MINT, None, USDC_DECIMALS, 0);
        seed_spl_mint_if_missing(
            &mut context.svm,
            loyal_actions::USDE_MINT,
            None,
            USDE_DECIMALS,
            0,
        );

        let usdc_source_0 = Pubkey::new_unique();
        let usdc_source_1 = Pubkey::new_unique();
        let usde_source_0 = Pubkey::new_unique();
        let usde_source_1 = Pubkey::new_unique();
        seed_spl_token_account(
            &mut context.svm,
            usdc_source_0,
            USDC_MINT,
            vault_0,
            1_000_000,
        );
        seed_spl_token_account(&mut context.svm, usdc_source_1, USDC_MINT, vault_1, 100_000);
        seed_spl_token_account(
            &mut context.svm,
            usde_source_0,
            loyal_actions::USDE_MINT,
            vault_0,
            1_000_000_000,
        );
        seed_spl_token_account(
            &mut context.svm,
            usde_source_1,
            loyal_actions::USDE_MINT,
            vault_1,
            100_000_000,
        );

        let (policy, create_policy_ix) =
            create_first_squads_internal_fund_transfer_policy_instruction(
                context.pool.settings,
                context.wallet_pubkey(),
                wallet_b.pubkey(),
            );
        try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
            .expect("wallet A creates InternalFundTransfer policy for wallet B");

        Some(Self {
            context,
            wallet_b,
            policy,
            vault_0,
            vault_1,
            usdc_source_0,
            usdc_source_1,
            usde_source_0,
            usde_source_1,
        })
    }

    #[allow(clippy::too_many_arguments)]
    fn transfer(
        &mut self,
        source_index: u8,
        destination_index: u8,
        source_token_account: Pubkey,
        destination_token_account: Pubkey,
        mint: Pubkey,
        decimals: u8,
        amount: u64,
    ) -> Result<(), String> {
        let ix = execute_squads_internal_fund_transfer_instruction(
            self.policy,
            self.wallet_b.pubkey(),
            self.context.pool.settings,
            source_index,
            destination_index,
            source_token_account,
            destination_token_account,
            mint,
            decimals,
            amount,
        );
        try_send_instructions(&mut self.context.svm, &[ix], &self.wallet_b, &[])
    }

    fn transfer_from_mint_account(
        &mut self,
        source_index: u8,
        destination_index: u8,
        source_token_account: Pubkey,
        destination_token_account: Pubkey,
        mint: Pubkey,
        amount: u64,
    ) -> Result<(), String> {
        let ix = execute_squads_internal_fund_transfer_instruction_from_mint_account(
            &self.context.svm,
            self.policy,
            self.wallet_b.pubkey(),
            self.context.pool.settings,
            source_index,
            destination_index,
            source_token_account,
            destination_token_account,
            mint,
            amount,
        );
        try_send_instructions(&mut self.context.svm, &[ix], &self.wallet_b, &[])
    }

    fn transfer_with_accounts(
        &mut self,
        account_index: u8,
        payload: SquadsInternalFundTransferPayload,
        accounts: Vec<AccountMeta>,
    ) -> Result<(), String> {
        let ix = execute_squads_internal_fund_transfer_instruction_with_accounts(
            self.policy,
            self.wallet_b.pubkey(),
            account_index,
            payload,
            accounts,
        );
        try_send_instructions(&mut self.context.svm, &[ix], &self.wallet_b, &[])
    }

    fn assert_rejected_without_balance_change(
        &mut self,
        label: &str,
        source_token_account: Pubkey,
        destination_token_account: Pubkey,
        attempt: impl FnOnce(&mut Self) -> Result<(), String>,
    ) {
        let source_before = get_spl_token_amount(&self.context.svm, source_token_account);
        let destination_before = get_spl_token_amount(&self.context.svm, destination_token_account);
        let result = attempt(self);
        assert!(result.is_err(), "{label} should be rejected");
        assert_eq!(
            get_spl_token_amount(&self.context.svm, source_token_account),
            source_before,
            "{label} source balance changed"
        );
        assert_eq!(
            get_spl_token_amount(&self.context.svm, destination_token_account),
            destination_before,
            "{label} destination balance changed"
        );
    }

    fn valid_usdc_accounts(&self) -> Vec<AccountMeta> {
        vec![
            AccountMeta::new_readonly(self.vault_0, false),
            AccountMeta::new(self.usdc_source_0, false),
            AccountMeta::new(self.usdc_source_1, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ]
    }
}

#[test]
fn wallet_b_can_move_allowed_stables_between_subaccounts() {
    let Some(mut fixture) = InternalFundTransferFixture::new() else {
        eprintln!("skipping InternalFundTransfer policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    fixture
        .transfer_from_mint_account(
            0,
            1,
            fixture.usdc_source_0,
            fixture.usdc_source_1,
            USDC_MINT,
            250_000,
        )
        .expect("wallet B moves USDC from subaccount 0 to 1");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.usdc_source_0),
        750_000
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.usdc_source_1),
        350_000
    );

    fixture
        .transfer_from_mint_account(
            1,
            0,
            fixture.usdc_source_1,
            fixture.usdc_source_0,
            USDC_MINT,
            125_000,
        )
        .expect("wallet B moves USDC from subaccount 1 to 0");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.usdc_source_0),
        875_000
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.usdc_source_1),
        225_000
    );

    fixture
        .transfer_from_mint_account(
            0,
            1,
            fixture.usde_source_0,
            fixture.usde_source_1,
            loyal_actions::USDE_MINT,
            250_000_000,
        )
        .expect("wallet B moves an additional supported 9-decimal stable");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.usde_source_0),
        750_000_000
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.usde_source_1),
        350_000_000
    );
}

#[test]
fn internal_fund_transfer_allowlist_matches_supported_stable_universe() {
    let stable_mints = supported_internal_fund_transfer_stable_mints();
    for mint in [
        USDC_MINT,
        loyal_actions::USDT_MINT,
        loyal_actions::PYUSD_MINT,
        loyal_actions::USDS_MINT,
        loyal_actions::USDG_MINT,
        loyal_actions::USDE_MINT,
        loyal_actions::SUSDE_MINT,
        loyal_actions::CASH_MINT,
        loyal_actions::SYRUP_USDC_MINT,
        loyal_actions::USD1_MINT,
        loyal_actions::FDUSD_MINT,
        loyal_actions::AUSD_MINT,
        loyal_actions::EUSX_MINT,
        loyal_actions::USCC_MINT,
    ] {
        assert!(
            stable_mints.contains(&mint),
            "{mint} missing from allowlist"
        );
    }
    assert!(!stable_mints.contains(&loyal_actions::USDH_MINT));
    assert!(!stable_mints.contains(&Pubkey::default()));
}

#[test]
fn internal_fund_transfer_policy_rejects_index_and_amount_invariants() {
    let Some(mut fixture) = InternalFundTransferFixture::new() else {
        eprintln!("skipping InternalFundTransfer policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    fixture.assert_rejected_without_balance_change(
        "source index outside allowlist",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer_with_accounts(
                2,
                SquadsInternalFundTransferPayload {
                    source_index: 2,
                    destination_index: 1,
                    mint: USDC_MINT,
                    decimals: USDC_DECIMALS,
                    amount: 1,
                },
                fixture.valid_usdc_accounts(),
            )
        },
    );

    fixture.assert_rejected_without_balance_change(
        "destination index outside allowlist",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer_with_accounts(
                0,
                SquadsInternalFundTransferPayload {
                    source_index: 0,
                    destination_index: 2,
                    mint: USDC_MINT,
                    decimals: USDC_DECIMALS,
                    amount: 1,
                },
                fixture.valid_usdc_accounts(),
            )
        },
    );

    fixture.assert_rejected_without_balance_change(
        "same source and destination index",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer_with_accounts(
                0,
                SquadsInternalFundTransferPayload {
                    source_index: 0,
                    destination_index: 0,
                    mint: USDC_MINT,
                    decimals: USDC_DECIMALS,
                    amount: 1,
                },
                fixture.valid_usdc_accounts(),
            )
        },
    );

    fixture.assert_rejected_without_balance_change(
        "zero amount",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer(
                0,
                1,
                fixture.usdc_source_0,
                fixture.usdc_source_1,
                USDC_MINT,
                USDC_DECIMALS,
                0,
            )
        },
    );
}

#[test]
fn internal_fund_transfer_policy_rejects_mints_outside_allowlist() {
    let Some(mut fixture) = InternalFundTransferFixture::new() else {
        eprintln!("skipping InternalFundTransfer policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let disallowed_mint = Pubkey::new_unique();
    let disallowed_source = Pubkey::new_unique();
    let disallowed_destination = Pubkey::new_unique();
    seed_spl_mint_if_missing(
        &mut fixture.context.svm,
        disallowed_mint,
        None,
        USDC_DECIMALS,
        0,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        disallowed_source,
        disallowed_mint,
        fixture.vault_0,
        100,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        disallowed_destination,
        disallowed_mint,
        fixture.vault_1,
        0,
    );
    fixture.assert_rejected_without_balance_change(
        "mint outside stable allowlist",
        disallowed_source,
        disallowed_destination,
        |fixture| {
            fixture.transfer(
                0,
                1,
                disallowed_source,
                disallowed_destination,
                disallowed_mint,
                USDC_DECIMALS,
                1,
            )
        },
    );
}

#[test]
fn internal_fund_transfer_policy_rejects_token_account_owner_and_mint_mismatches() {
    let Some(mut fixture) = InternalFundTransferFixture::new() else {
        eprintln!("skipping InternalFundTransfer policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let wrong_owner_source = Pubkey::new_unique();
    let wallet_owner = fixture.context.wallet_pubkey();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_owner_source,
        USDC_MINT,
        wallet_owner,
        100,
    );
    fixture.assert_rejected_without_balance_change(
        "source token account wrong owner",
        wrong_owner_source,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer(
                0,
                1,
                wrong_owner_source,
                fixture.usdc_source_1,
                USDC_MINT,
                USDC_DECIMALS,
                1,
            )
        },
    );

    let wrong_owner_destination = Pubkey::new_unique();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_owner_destination,
        USDC_MINT,
        wallet_owner,
        0,
    );
    fixture.assert_rejected_without_balance_change(
        "destination token account wrong owner",
        fixture.usdc_source_0,
        wrong_owner_destination,
        |fixture| {
            fixture.transfer(
                0,
                1,
                fixture.usdc_source_0,
                wrong_owner_destination,
                USDC_MINT,
                USDC_DECIMALS,
                1,
            )
        },
    );

    let wrong_mint_source = Pubkey::new_unique();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_mint_source,
        loyal_actions::USDE_MINT,
        fixture.vault_0,
        100,
    );
    fixture.assert_rejected_without_balance_change(
        "source token account wrong mint",
        wrong_mint_source,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer(
                0,
                1,
                wrong_mint_source,
                fixture.usdc_source_1,
                USDC_MINT,
                USDC_DECIMALS,
                1,
            )
        },
    );

    let wrong_mint_destination = Pubkey::new_unique();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_mint_destination,
        loyal_actions::USDE_MINT,
        fixture.vault_1,
        0,
    );
    fixture.assert_rejected_without_balance_change(
        "destination token account wrong mint",
        fixture.usdc_source_0,
        wrong_mint_destination,
        |fixture| {
            fixture.transfer(
                0,
                1,
                fixture.usdc_source_0,
                wrong_mint_destination,
                USDC_MINT,
                USDC_DECIMALS,
                1,
            )
        },
    );
}

#[test]
fn internal_fund_transfer_policy_rejects_wrong_pda_and_program_accounts() {
    let Some(mut fixture) = InternalFundTransferFixture::new() else {
        eprintln!("skipping InternalFundTransfer policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    fixture.assert_rejected_without_balance_change(
        "wrong source smart-account PDA",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            let mut accounts = fixture.valid_usdc_accounts();
            accounts[0] = AccountMeta::new_readonly(fixture.vault_1, false);
            fixture.transfer_with_accounts(
                0,
                SquadsInternalFundTransferPayload {
                    source_index: 0,
                    destination_index: 1,
                    mint: USDC_MINT,
                    decimals: USDC_DECIMALS,
                    amount: 1,
                },
                accounts,
            )
        },
    );

    fixture.assert_rejected_without_balance_change(
        "wrong token program",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            let mut accounts = fixture.valid_usdc_accounts();
            accounts[4] = AccountMeta::new_readonly(solana_sdk::system_program::ID, false);
            fixture.transfer_with_accounts(
                0,
                SquadsInternalFundTransferPayload {
                    source_index: 0,
                    destination_index: 1,
                    mint: USDC_MINT,
                    decimals: USDC_DECIMALS,
                    amount: 1,
                },
                accounts,
            )
        },
    );
}

#[test]
fn internal_fund_transfer_policy_rejects_execution_after_policy_removal() {
    let Some(mut fixture) = InternalFundTransferFixture::new() else {
        eprintln!("skipping InternalFundTransfer policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let remove_policy_ix = remove_squads_policy_instruction(
        fixture.context.pool.settings,
        fixture.context.wallet_pubkey(),
        fixture.policy,
    );
    try_send_instructions(
        &mut fixture.context.svm,
        &[remove_policy_ix],
        &fixture.context.wallet,
        &[],
    )
    .expect("wallet A removes InternalFundTransfer policy");

    fixture.assert_rejected_without_balance_change(
        "execution after policy removal",
        fixture.usdc_source_0,
        fixture.usdc_source_1,
        |fixture| {
            fixture.transfer(
                0,
                1,
                fixture.usdc_source_0,
                fixture.usdc_source_1,
                USDC_MINT,
                USDC_DECIMALS,
                1,
            )
        },
    );
}
