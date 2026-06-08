use solana_sdk::{
    account::Account,
    pubkey::Pubkey,
    rent::Rent,
    signature::{Keypair, Signer},
};
use squads_test_harness::prelude::*;

const POLICY_SEED: u64 = 1;
const WALLET_USDC: u64 = 1_000_000;
const PERIOD_CAP: u64 = 250_000;
const SWEEP_AMOUNT: u64 = 100_000;
const PERIOD_LENGTH_S: u64 = 3_600;

struct SubscriptionSweepFixture {
    context: FundedSquadsTestContext,
    automation_signer: Keypair,
    policy: Pubkey,
    wallet_usdc: Pubkey,
    vault_usdc: Pubkey,
    subscription_authority: Pubkey,
}

struct CreatedDelegation {
    pubkey: Pubkey,
}

impl SubscriptionSweepFixture {
    fn new() -> Self {
        let mut context = create_funded_squads_test_context_with_config(FundedSquadsTestConfig {
            vault_index: 1,
            ..FundedSquadsTestConfig::default()
        })
        .expect("create funded Squads test context")
        .expect("Squads program fixture must be available");

        add_subscriptions_program_from_env_or_fixture(&mut context.svm)
            .expect("load Subscriptions program")
            .expect("Subscriptions program fixture or SUBSCRIPTIONS_PROGRAM_SO must be available");
        context.svm.set_sysvar(&Rent {
            lamports_per_byte_year: 1,
            exemption_threshold: 1.0,
            burn_percent: 0,
        });

        seed_spl_mint_if_missing(&mut context.svm, USDC_MINT, None, USDC_DECIMALS, 0);

        let wallet = context.wallet_pubkey();
        let vault = context.vault;
        let wallet_usdc = derive_associated_token_account(wallet, USDC_MINT);
        let vault_usdc = derive_associated_token_account(vault, USDC_MINT);
        seed_spl_token_account(
            &mut context.svm,
            wallet_usdc,
            USDC_MINT,
            wallet,
            WALLET_USDC,
        );
        seed_spl_token_account(&mut context.svm, vault_usdc, USDC_MINT, vault, 0);

        let subscription_authority = derive_subscription_authority(wallet, USDC_MINT);
        let init_ix = subscription_init_authority_instruction(
            wallet,
            subscription_authority,
            USDC_MINT,
            wallet_usdc,
        );
        try_send_instructions(&mut context.svm, &[init_ix], &context.wallet, &[])
            .expect("initialize subscription authority");

        let automation_signer = Keypair::new();
        context
            .svm
            .airdrop(&automation_signer.pubkey(), LAMPORTS_PER_SOL / 10)
            .expect("airdrop automation signer");

        let (policy, _) = derive_squads_policy(&context.pool.settings, POLICY_SEED);
        let create_policy_ix =
            create_squads_program_interaction_flexible_subscription_sweep_policy_instruction(
                context.pool.settings,
                wallet,
                automation_signer.pubkey(),
                POLICY_SEED,
                context.vault_index,
                wallet,
                vault,
                wallet_usdc,
                vault_usdc,
                PERIOD_CAP,
            );
        try_send_instructions(&mut context.svm, &[create_policy_ix], &context.wallet, &[])
            .expect("create subscription sweep policy");

        Self {
            context,
            automation_signer,
            policy,
            wallet_usdc,
            vault_usdc,
            subscription_authority,
        }
    }

    fn create_delegation(
        &mut self,
        nonce: u64,
        amount_per_period: u64,
        period_length_s: u64,
        expiry_ts: i64,
    ) -> CreatedDelegation {
        let wallet = self.context.wallet_pubkey();
        let delegation = derive_recurring_delegation(
            self.subscription_authority,
            wallet,
            self.context.vault,
            nonce,
        );
        let init_id = subscription_authority_init_id(&self.context);
        let create_delegation_ix = subscription_create_recurring_delegation_instruction(
            SubscriptionRecurringDelegationArgs {
                delegator: wallet,
                subscription_authority: self.subscription_authority,
                delegation,
                delegatee: self.context.vault,
                nonce,
                amount_per_period,
                period_length_s,
                start_ts: 0,
                expiry_ts,
                expected_subscription_authority_init_id: init_id,
            },
        );
        try_send_instructions(
            &mut self.context.svm,
            &[create_delegation_ix],
            &self.context.wallet,
            &[],
        )
        .expect("create recurring delegation");
        assert_recurring_delegation_layout(
            &self.context,
            delegation,
            wallet,
            self.context.vault,
            self.subscription_authority,
            USDC_MINT,
            amount_per_period,
        );

        CreatedDelegation { pubkey: delegation }
    }

    fn transfer_ix(&self, delegation: Pubkey, amount: u64) -> solana_sdk::instruction::Instruction {
        self.transfer_ix_with(delegation, amount, self.context.wallet_pubkey(), USDC_MINT)
    }

    fn transfer_ix_with(
        &self,
        delegation: Pubkey,
        amount: u64,
        delegator: Pubkey,
        mint: Pubkey,
    ) -> solana_sdk::instruction::Instruction {
        execute_squads_subscription_recurring_transfer_instruction(
            SubscriptionRecurringTransferExecution {
                policy: self.policy,
                signer: self.automation_signer.pubkey(),
                account_index: self.context.vault_index,
                delegation,
                subscription_authority: self.subscription_authority,
                delegator_ata: self.wallet_usdc,
                receiver_ata: self.vault_usdc,
                delegatee: self.context.vault,
                amount,
                delegator,
                mint,
            },
        )
    }

    fn send_transfer(&mut self, ix: solana_sdk::instruction::Instruction) -> Result<(), String> {
        try_send_instructions(&mut self.context.svm, &[ix], &self.automation_signer, &[])
    }

    fn assert_rejected_with_automation_without_balance_change(
        &mut self,
        label: &str,
        ix: solana_sdk::instruction::Instruction,
    ) {
        let wallet_before = get_spl_token_amount(&self.context.svm, self.wallet_usdc);
        let vault_before = get_spl_token_amount(&self.context.svm, self.vault_usdc);
        let result =
            try_send_instructions(&mut self.context.svm, &[ix], &self.automation_signer, &[]);
        assert!(result.is_err(), "{label} should be rejected");
        assert_eq!(
            get_spl_token_amount(&self.context.svm, self.wallet_usdc),
            wallet_before,
            "{label} changed wallet USDC"
        );
        assert_eq!(
            get_spl_token_amount(&self.context.svm, self.vault_usdc),
            vault_before,
            "{label} changed vault USDC"
        );
    }

    fn cloned_delegation_with(
        &mut self,
        source: Pubkey,
        edit: impl FnOnce(&mut Vec<u8>),
    ) -> Pubkey {
        let source_account = self
            .context
            .svm
            .get_account(&source)
            .expect("source delegation exists");
        let mut data = source_account.data.clone();
        edit(&mut data);

        let cloned = Pubkey::new_unique();
        self.context
            .svm
            .set_account(
                cloned,
                Account {
                    lamports: source_account.lamports,
                    data,
                    owner: SUBSCRIPTIONS_PROGRAM_ID,
                    executable: false,
                    rent_epoch: 0,
                },
            )
            .expect("seed cloned delegation account");
        cloned
    }
}

#[test]
fn flexible_policy_allows_rotated_recurring_delegation_nonces() {
    let mut fixture = SubscriptionSweepFixture::new();
    let delegation_a = fixture.create_delegation(7, PERIOD_CAP, PERIOD_LENGTH_S, 0);
    let delegation_b = fixture.create_delegation(8, PERIOD_CAP, PERIOD_LENGTH_S * 2, 86_400);
    let wallet_lamports_before = fixture.context.wallet_balance();
    let vault_lamports_before = fixture.context.vault_balance();

    let ix = fixture.transfer_ix(delegation_a.pubkey, SWEEP_AMOUNT);
    fixture
        .send_transfer(ix)
        .expect("subscription sweep transfers from first recurring delegation");

    let ix = fixture.transfer_ix(delegation_b.pubkey, SWEEP_AMOUNT);
    fixture
        .send_transfer(ix)
        .expect("same policy transfers from rotated recurring delegation");

    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.wallet_usdc),
        WALLET_USDC - (SWEEP_AMOUNT * 2)
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        SWEEP_AMOUNT * 2
    );
    assert_eq!(
        recurring_delegation_amount_pulled_in_period(&fixture.context.svm, delegation_a.pubkey),
        SWEEP_AMOUNT
    );
    assert_eq!(
        recurring_delegation_amount_pulled_in_period(&fixture.context.svm, delegation_b.pubkey),
        SWEEP_AMOUNT
    );
    assert_eq!(fixture.context.wallet_balance(), wallet_lamports_before);
    assert_eq!(fixture.context.vault_balance(), vault_lamports_before);
}

#[test]
fn subscription_sweep_policy_rejects_unapproved_transfer_shapes() {
    let mut fixture = SubscriptionSweepFixture::new();
    let delegation = fixture.create_delegation(7, PERIOD_CAP, PERIOD_LENGTH_S, 0);

    let over_transfer_amount = fixture.transfer_ix(delegation.pubkey, PERIOD_CAP + 1);
    fixture.assert_rejected_with_automation_without_balance_change(
        "above delegation cap transfer",
        over_transfer_amount,
    );

    let over_policy_cap = fixture.create_delegation(8, PERIOD_CAP + 1, PERIOD_LENGTH_S, 0);
    let ix = fixture.transfer_ix(over_policy_cap.pubkey, SWEEP_AMOUNT);
    fixture.assert_rejected_with_automation_without_balance_change("above policy cap", ix);

    let wrong_receiver = Pubkey::new_unique();
    seed_spl_token_account(
        &mut fixture.context.svm,
        wrong_receiver,
        USDC_MINT,
        fixture.context.vault,
        0,
    );
    let mut ix = fixture.transfer_ix(delegation.pubkey, SWEEP_AMOUNT);
    ix.accounts[3 + 3].pubkey = wrong_receiver;
    fixture.assert_rejected_with_automation_without_balance_change("wrong receiver", ix);

    let wrong_mint = Pubkey::new_unique();
    seed_spl_mint_if_missing(&mut fixture.context.svm, wrong_mint, None, USDC_DECIMALS, 0);
    let mut ix = fixture.transfer_ix_with(
        delegation.pubkey,
        SWEEP_AMOUNT,
        fixture.context.wallet_pubkey(),
        wrong_mint,
    );
    ix.accounts[3 + 4].pubkey = wrong_mint;
    fixture.assert_rejected_with_automation_without_balance_change("wrong mint", ix);

    let wrong_delegator_data = fixture.cloned_delegation_with(delegation.pubkey, |data| {
        write_pubkey(
            data,
            SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET,
            Pubkey::new_unique(),
        );
    });
    let ix = fixture.transfer_ix(wrong_delegator_data, SWEEP_AMOUNT);
    fixture
        .assert_rejected_with_automation_without_balance_change("wrong delegator account data", ix);

    let wrong_delegatee_data = fixture.cloned_delegation_with(delegation.pubkey, |data| {
        write_pubkey(
            data,
            SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET,
            Pubkey::new_unique(),
        );
    });
    let ix = fixture.transfer_ix(wrong_delegatee_data, SWEEP_AMOUNT);
    fixture
        .assert_rejected_with_automation_without_balance_change("wrong delegatee account data", ix);

    let wrong_mint_data = fixture.cloned_delegation_with(delegation.pubkey, |data| {
        write_pubkey(
            data,
            SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET,
            Pubkey::new_unique(),
        );
    });
    let ix = fixture.transfer_ix(wrong_mint_data, SWEEP_AMOUNT);
    fixture.assert_rejected_with_automation_without_balance_change("wrong mint account data", ix);

    let wrong_authority_data = fixture.cloned_delegation_with(delegation.pubkey, |data| {
        write_pubkey(
            data,
            SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET,
            Pubkey::new_unique(),
        );
    });
    let ix = fixture.transfer_ix(wrong_authority_data, SWEEP_AMOUNT);
    fixture
        .assert_rejected_with_automation_without_balance_change("wrong authority account data", ix);

    let fixed_or_plan_delegation = fixture.cloned_delegation_with(delegation.pubkey, |data| {
        data[usize_offset(SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET)] =
            SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR - 1;
    });
    let ix = fixture.transfer_ix(fixed_or_plan_delegation, SWEEP_AMOUNT);
    fixture.assert_rejected_with_automation_without_balance_change(
        "non-recurring delegation discriminator",
        ix,
    );

    let wrong_delegator = Pubkey::new_unique();
    let ix = fixture.transfer_ix_with(delegation.pubkey, SWEEP_AMOUNT, wrong_delegator, USDC_MINT);
    fixture.assert_rejected_with_automation_without_balance_change(
        "wrong delegator transfer data",
        ix,
    );

    let outsider = Keypair::new();
    fixture
        .context
        .svm
        .airdrop(&outsider.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop outsider");
    let mut ix = fixture.transfer_ix(delegation.pubkey, SWEEP_AMOUNT);
    ix.accounts[2].pubkey = outsider.pubkey();
    let wallet_before = get_spl_token_amount(&fixture.context.svm, fixture.wallet_usdc);
    let vault_before = get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc);
    let result = try_send_instructions(&mut fixture.context.svm, &[ix], &outsider, &[]);
    assert!(result.is_err(), "external signer should be rejected");
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.wallet_usdc),
        wallet_before
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        vault_before
    );
}

#[test]
fn revoked_delegation_or_removed_policy_stops_subscription_sweep() {
    let mut revoked = SubscriptionSweepFixture::new();
    let delegation = revoked.create_delegation(7, PERIOD_CAP, PERIOD_LENGTH_S, 0);
    let revoke_ix = subscription_revoke_delegation_instruction(
        revoked.context.wallet_pubkey(),
        delegation.pubkey,
    );
    try_send_instructions(
        &mut revoked.context.svm,
        &[revoke_ix],
        &revoked.context.wallet,
        &[],
    )
    .expect("revoke recurring delegation");
    let ix = revoked.transfer_ix(delegation.pubkey, SWEEP_AMOUNT);
    revoked.assert_rejected_with_automation_without_balance_change("revoked delegation", ix);

    let mut removed = SubscriptionSweepFixture::new();
    let delegation = removed.create_delegation(7, PERIOD_CAP, PERIOD_LENGTH_S, 0);
    let remove_policy_ix = remove_squads_policy_instruction(
        removed.context.pool.settings,
        removed.context.wallet_pubkey(),
        removed.policy,
    );
    try_send_instructions(
        &mut removed.context.svm,
        &[remove_policy_ix],
        &removed.context.wallet,
        &[],
    )
    .expect("remove Squads policy");
    let ix = removed.transfer_ix(delegation.pubkey, SWEEP_AMOUNT);
    removed.assert_rejected_with_automation_without_balance_change("removed Squads policy", ix);
}

fn assert_recurring_delegation_layout(
    context: &FundedSquadsTestContext,
    delegation: Pubkey,
    delegator: Pubkey,
    delegatee: Pubkey,
    subscription_authority: Pubkey,
    mint: Pubkey,
    amount_per_period: u64,
) {
    let account = context
        .svm
        .get_account(&delegation)
        .expect("recurring delegation account exists");
    assert_eq!(account.owner, SUBSCRIPTIONS_PROGRAM_ID);
    assert_eq!(
        account.data.len(),
        SUBSCRIPTION_RECURRING_DELEGATION_DATA_LEN,
        "recurring delegation layout length drifted"
    );
    assert_eq!(
        account.data[usize_offset(SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET)],
        SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR
    );
    assert_eq!(
        read_pubkey(
            &account.data,
            SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET
        ),
        delegator
    );
    assert_eq!(
        read_pubkey(
            &account.data,
            SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET
        ),
        delegatee
    );
    assert_eq!(
        read_pubkey(
            &account.data,
            SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET
        ),
        subscription_authority
    );
    assert_eq!(
        read_pubkey(&account.data, SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET),
        mint
    );
    assert_eq!(
        read_u64(
            &account.data,
            SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET
        ),
        amount_per_period
    );
}

fn subscription_authority_init_id(context: &FundedSquadsTestContext) -> i64 {
    const SUBSCRIPTION_AUTHORITY_INIT_ID_OFFSET: usize = 98;
    let authority = derive_subscription_authority(context.wallet_pubkey(), USDC_MINT);
    let account = context
        .svm
        .get_account(&authority)
        .expect("subscription authority exists");
    let bytes = account
        .data
        .get(SUBSCRIPTION_AUTHORITY_INIT_ID_OFFSET..SUBSCRIPTION_AUTHORITY_INIT_ID_OFFSET + 8)
        .expect("subscription authority init id exists");
    i64::from_le_bytes(bytes.try_into().expect("init id field is 8 bytes"))
}

fn write_pubkey(data: &mut [u8], offset: u64, pubkey: Pubkey) {
    let offset = usize_offset(offset);
    data[offset..offset + 32].copy_from_slice(pubkey.as_ref());
}

fn read_pubkey(data: &[u8], offset: u64) -> Pubkey {
    let offset = usize_offset(offset);
    Pubkey::new_from_array(
        data[offset..offset + 32]
            .try_into()
            .expect("pubkey field is 32 bytes"),
    )
}

fn read_u64(data: &[u8], offset: u64) -> u64 {
    let offset = usize_offset(offset);
    u64::from_le_bytes(
        data[offset..offset + 8]
            .try_into()
            .expect("u64 field is 8 bytes"),
    )
}

fn usize_offset(offset: u64) -> usize {
    usize::try_from(offset).expect("offset fits in usize")
}
