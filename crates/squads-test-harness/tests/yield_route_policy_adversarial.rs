use loyal_actions::{
    create_all_in_one_market_mint_yield_route_action, LoyalActionContext, SwapLane,
    YieldRouteActionSetup,
};
use solana_sdk::{
    account::Account, instruction::AccountMeta, pubkey::Pubkey, signature::Keypair, signer::Signer,
};
use spl_token::solana_program::program_pack::Pack;
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs, derive_loyal_hub_authority,
    derive_squads_vault, execute_squads_program_interaction_instruction,
    execute_squads_sync_transaction_instruction, get_spl_token_amount,
    initialize_loyal_hub_config_instruction, loyal_action_context, loyal_hub_token_account,
    mock_jupiter_stable_exact_in_swap_data, mock_jupiter_stable_exact_in_swap_data_with_slippage,
    mock_jupiter_stable_reserve_token_account, mock_jupiter_swap_data, mock_jupiter_swap_lane,
    mock_kamino_deposit_reserve_liquidity_data, mock_kamino_reserve_transaction,
    mock_kamino_withdraw_reserve_liquidity_data, remove_squads_policy_instruction,
    seed_empty_system_account_if_missing, seed_loyal_hub_inventory_spl_accounts,
    seed_mock_jupiter_stable_reserve_spl_accounts, seed_mock_kamino_reserve_spl_accounts,
    seed_mock_kamino_reserve_spl_accounts_with_mint, seed_spl_mint_if_missing,
    seed_spl_token_account, try_send_instructions, try_send_instructions_with_heap_frame,
    yield_route_universe_from_mock_reserves, FundedSquadsTestContext, HubRouteExecution,
    HubSwapExecution, JupiterRouteExecution, JupiterSwapExecution,
    MockJupiterStableReserveTokenAccount, MockKaminoReserveTokenAccounts, MockProgram,
    RouteActionExt, SquadsCompiledInstruction, JUPITER_V6_PROGRAM_ID, KAMINO_LEND_PROGRAM_ID,
    KAMINO_MAIN_MARKET, KAMINO_MAIN_PYUSD_RESERVE, KAMINO_MAIN_USDC_RESERVE, KAMINO_PRIME_MARKET,
    KAMINO_PRIME_USDC_RESERVE, LAMPORTS_PER_SOL, LOYAL_HUB_SWAP_PROGRAM_ID,
    MOCK_JUPITER_USDC_TO_PYUSD, PYUSD_DECIMALS, PYUSD_MINT, USDC_DECIMALS, USDC_MINT,
};

const AMOUNT: u64 = 1_000_000;
const HUB_OUT: u64 = 995_000;
const ADVERSARIAL_HUB_DUST_OUT: u64 = 1;
const MAX_FEE_BPS: u16 = 50;

struct AdversarialRouteFixture {
    context: FundedSquadsTestContext,
    wallet_b: Keypair,
    hub_authorizer: Keypair,
    route_action: YieldRouteActionSetup,
    attacker: Pubkey,
    second_vault: Pubkey,
    vault_usdc: Pubkey,
    vault_pyusd: Pubkey,
    vault_main_usdc_collateral: Pubkey,
    vault_prime_usdc_collateral: Pubkey,
    vault_pyusd_collateral: Pubkey,
    main_usdc_reserve: MockKaminoReserveTokenAccounts,
    prime_usdc_reserve: MockKaminoReserveTokenAccounts,
    pyusd_reserve: MockKaminoReserveTokenAccounts,
    attacker_usdc: Pubkey,
    attacker_pyusd: Pubkey,
    attacker_main_usdc_collateral: Pubkey,
    attacker_prime_usdc_collateral: Pubkey,
    second_vault_usdc: Pubkey,
    second_vault_pyusd: Pubkey,
    second_vault_main_usdc_collateral: Pubkey,
    second_vault_prime_usdc_collateral: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct CustodySnapshot(Vec<CustodyAccountSnapshot>);

#[derive(Clone, Debug, PartialEq, Eq)]
struct CustodyAccountSnapshot {
    label: String,
    pubkey: Pubkey,
    owner: Option<Pubkey>,
    lamports: Option<u64>,
    data_len: Option<usize>,
    token_amount: Option<u64>,
}

#[derive(Debug)]
struct RawRoutePayload {
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    transaction_accounts: Vec<AccountMeta>,
}

struct MutationCase {
    name: &'static str,
    payload_builder: fn(&AdversarialRouteFixture) -> RawRoutePayload,
    mutate: fn(&AdversarialRouteFixture, &mut RawRoutePayload),
    include_hub_authorizer: bool,
    expected_boundary: &'static str,
}

impl AdversarialRouteFixture {
    fn setup() -> Option<Self> {
        Self::setup_with_hub_program(MockProgram::LoyalHubSwap)
    }

    fn setup_with_adversarial_hub_program() -> Option<Self> {
        Self::setup_with_hub_program(MockProgram::AdversarialLoyalHubSwap)
    }

    fn setup_with_hub_program(hub_program: MockProgram) -> Option<Self> {
        let mut context = create_funded_squads_test_context_with_mock_programs(&[
            MockProgram::Jupiter,
            MockProgram::KaminoLend,
            hub_program,
        ])
        .expect("create funded Squads test context")?;

        let wallet_b = Keypair::new();
        let hub_authorizer = Keypair::new();
        let attacker = Keypair::new().pubkey();
        let second_vault = derive_squads_vault(&context.pool.settings, context.vault_index + 1).0;
        context
            .svm
            .airdrop(&wallet_b.pubkey(), LAMPORTS_PER_SOL / 10)
            .expect("airdrop wallet B");
        context
            .svm
            .airdrop(&hub_authorizer.pubkey(), LAMPORTS_PER_SOL / 10)
            .expect("airdrop hub authorizer");

        seed_mock_jupiter_stable_reserve_spl_accounts(
            &mut context.svm,
            &[
                MockJupiterStableReserveTokenAccount {
                    mint: USDC_MINT,
                    reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
                },
                MockJupiterStableReserveTokenAccount {
                    mint: PYUSD_MINT,
                    reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
                },
            ],
            10 * AMOUNT,
        );
        seed_loyal_hub_inventory_spl_accounts(&mut context.svm, &[USDC_MINT, PYUSD_MINT], 0);
        seed_spl_token_account(
            &mut context.svm,
            loyal_hub_token_account(PYUSD_MINT),
            PYUSD_MINT,
            derive_loyal_hub_authority(),
            10 * AMOUNT,
        );

        let vault_usdc = Keypair::new().pubkey();
        let vault_pyusd = Keypair::new().pubkey();
        let vault_main_usdc_collateral = Keypair::new().pubkey();
        let vault_prime_usdc_collateral = Keypair::new().pubkey();
        let vault_pyusd_collateral = Keypair::new().pubkey();
        let main_usdc_supply = Keypair::new().pubkey();
        let prime_usdc_supply = Keypair::new().pubkey();
        let pyusd_supply = Keypair::new().pubkey();

        seed_spl_token_account(
            &mut context.svm,
            vault_usdc,
            USDC_MINT,
            context.vault,
            AMOUNT,
        );
        let main_usdc_reserve = seed_mock_kamino_reserve_spl_accounts(
            &mut context.svm,
            KAMINO_MAIN_USDC_RESERVE,
            KAMINO_MAIN_MARKET,
            context.vault,
            vault_usdc,
            vault_main_usdc_collateral,
            main_usdc_supply,
        );
        let prime_usdc_reserve = seed_mock_kamino_reserve_spl_accounts(
            &mut context.svm,
            KAMINO_PRIME_USDC_RESERVE,
            KAMINO_PRIME_MARKET,
            context.vault,
            vault_usdc,
            vault_prime_usdc_collateral,
            prime_usdc_supply,
        );
        let pyusd_reserve = seed_mock_kamino_reserve_spl_accounts_with_mint(
            &mut context.svm,
            KAMINO_MAIN_PYUSD_RESERVE,
            KAMINO_MAIN_MARKET,
            PYUSD_MINT,
            PYUSD_DECIMALS,
            context.vault,
            vault_pyusd,
            vault_pyusd_collateral,
            pyusd_supply,
        );

        let attacker_usdc = Keypair::new().pubkey();
        let attacker_pyusd = Keypair::new().pubkey();
        let attacker_main_usdc_collateral = Keypair::new().pubkey();
        let attacker_prime_usdc_collateral = Keypair::new().pubkey();
        let second_vault_usdc = Keypair::new().pubkey();
        let second_vault_pyusd = Keypair::new().pubkey();
        let second_vault_main_usdc_collateral = Keypair::new().pubkey();
        let second_vault_prime_usdc_collateral = Keypair::new().pubkey();

        for (account, mint, owner, amount) in [
            (attacker_usdc, USDC_MINT, attacker, AMOUNT),
            (attacker_pyusd, PYUSD_MINT, attacker, 0),
            (
                attacker_main_usdc_collateral,
                main_usdc_reserve.collateral_mint,
                attacker,
                AMOUNT,
            ),
            (
                attacker_prime_usdc_collateral,
                prime_usdc_reserve.collateral_mint,
                attacker,
                0,
            ),
            (second_vault_usdc, USDC_MINT, second_vault, AMOUNT),
            (second_vault_pyusd, PYUSD_MINT, second_vault, 0),
            (
                second_vault_main_usdc_collateral,
                main_usdc_reserve.collateral_mint,
                second_vault,
                AMOUNT,
            ),
            (
                second_vault_prime_usdc_collateral,
                prime_usdc_reserve.collateral_mint,
                second_vault,
                0,
            ),
        ] {
            seed_spl_token_account(&mut context.svm, account, mint, owner, amount);
        }

        match hub_program {
            MockProgram::LoyalHubSwap => {
                let init_hub_ix = initialize_loyal_hub_config_instruction(
                    hub_authorizer.pubkey(),
                    hub_authorizer.pubkey(),
                    hub_authorizer.pubkey(),
                    MAX_FEE_BPS,
                    false,
                    &[USDC_MINT, PYUSD_MINT],
                );
                try_send_instructions(&mut context.svm, &[init_hub_ix], &hub_authorizer, &[])
                    .expect("hub authorizer initializes Loyal Hub config");
            }
            MockProgram::AdversarialLoyalHubSwap => {
                context
                    .svm
                    .set_account(
                        loyal_actions::derive_loyal_hub_config(),
                        Account {
                            lamports: LAMPORTS_PER_SOL,
                            data: vec![0; loyal_actions::hub_abi::config_account::MAX_LEN],
                            owner: LOYAL_HUB_SWAP_PROGRAM_ID,
                            executable: false,
                            rent_epoch: 0,
                        },
                    )
                    .expect("seed adversarial Hub config account");
            }
            MockProgram::Jupiter | MockProgram::KaminoLend => unreachable!(),
        }

        let route_action = create_all_in_one_market_mint_yield_route_action(
            loyal_action_context(&context, wallet_b.pubkey()),
            yield_route_universe_from_mock_reserves(
                vec![USDC_MINT, PYUSD_MINT],
                vec![main_usdc_reserve, prime_usdc_reserve, pyusd_reserve],
            ),
            vec![
                mock_jupiter_swap_lane(false),
                SwapLane::LoyalHub {
                    hub_authorizer: hub_authorizer.pubkey(),
                    max_fee_bps: MAX_FEE_BPS,
                },
            ],
        )
        .expect("build all-in-one route action");
        try_send_instructions_with_heap_frame(
            &mut context.svm,
            &route_action.instructions,
            &context.wallet,
            &[],
        )
        .expect("wallet A creates all-in-one route policy");

        let (main_deposit_instructions, main_deposit_accounts) = mock_kamino_reserve_transaction(
            context.vault,
            main_usdc_reserve,
            mock_kamino_deposit_reserve_liquidity_data(AMOUNT),
        );
        let main_deposit_ix = execute_squads_sync_transaction_instruction(
            context.pool.settings,
            context.wallet_pubkey(),
            context.vault_index,
            main_deposit_instructions,
            main_deposit_accounts,
        );
        try_send_instructions(&mut context.svm, &[main_deposit_ix], &context.wallet, &[])
            .expect("wallet A deposits USDC into starting reserve");

        Some(Self {
            context,
            wallet_b,
            hub_authorizer,
            route_action,
            attacker,
            second_vault,
            vault_usdc,
            vault_pyusd,
            vault_main_usdc_collateral,
            vault_prime_usdc_collateral,
            vault_pyusd_collateral,
            main_usdc_reserve,
            prime_usdc_reserve,
            pyusd_reserve,
            attacker_usdc,
            attacker_pyusd,
            attacker_main_usdc_collateral,
            attacker_prime_usdc_collateral,
            second_vault_usdc,
            second_vault_pyusd,
            second_vault_main_usdc_collateral,
            second_vault_prime_usdc_collateral,
        })
    }

    fn main_withdraw(&self) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
        mock_kamino_reserve_transaction(
            self.context.vault,
            self.main_usdc_reserve,
            mock_kamino_withdraw_reserve_liquidity_data(AMOUNT),
        )
    }

    fn prime_deposit(&self, amount: u64) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
        mock_kamino_reserve_transaction(
            self.context.vault,
            self.prime_usdc_reserve,
            mock_kamino_deposit_reserve_liquidity_data(amount),
        )
    }

    fn pyusd_deposit(&self, amount: u64) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
        mock_kamino_reserve_transaction(
            self.context.vault,
            self.pyusd_reserve,
            mock_kamino_deposit_reserve_liquidity_data(amount),
        )
    }

    fn assert_starting_balances(&self) {
        assert_eq!(
            get_spl_token_amount(&self.context.svm, self.vault_main_usdc_collateral),
            AMOUNT
        );
        assert_eq!(get_spl_token_amount(&self.context.svm, self.vault_usdc), 0);
        assert_eq!(
            get_spl_token_amount(&self.context.svm, self.vault_prime_usdc_collateral),
            0
        );
        assert_eq!(
            get_spl_token_amount(&self.context.svm, self.vault_pyusd_collateral),
            0
        );
    }

    fn custody_accounts(&self) -> Vec<(&'static str, Pubkey)> {
        vec![
            ("squads_vault", self.context.vault),
            ("attacker_owner", self.attacker),
            ("second_squads_vault", self.second_vault),
            ("vault_usdc", self.vault_usdc),
            ("vault_pyusd", self.vault_pyusd),
            (
                "vault_main_usdc_collateral",
                self.vault_main_usdc_collateral,
            ),
            (
                "vault_prime_usdc_collateral",
                self.vault_prime_usdc_collateral,
            ),
            ("vault_pyusd_collateral", self.vault_pyusd_collateral),
            ("attacker_usdc", self.attacker_usdc),
            ("attacker_pyusd", self.attacker_pyusd),
            (
                "attacker_main_usdc_collateral",
                self.attacker_main_usdc_collateral,
            ),
            (
                "attacker_prime_usdc_collateral",
                self.attacker_prime_usdc_collateral,
            ),
            ("second_vault_usdc", self.second_vault_usdc),
            ("second_vault_pyusd", self.second_vault_pyusd),
            (
                "second_vault_main_usdc_collateral",
                self.second_vault_main_usdc_collateral,
            ),
            (
                "second_vault_prime_usdc_collateral",
                self.second_vault_prime_usdc_collateral,
            ),
            (
                "jupiter_usdc_reserve",
                mock_jupiter_stable_reserve_token_account(USDC_MINT),
            ),
            (
                "jupiter_pyusd_reserve",
                mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            ),
            ("hub_usdc_inventory", loyal_hub_token_account(USDC_MINT)),
            ("hub_pyusd_inventory", loyal_hub_token_account(PYUSD_MINT)),
            (
                "main_usdc_liquidity_supply",
                self.main_usdc_reserve.reserve_liquidity_supply,
            ),
            ("main_usdc_reserve", self.main_usdc_reserve.reserve),
            (
                "main_usdc_collateral_mint",
                self.main_usdc_reserve.collateral_mint,
            ),
            (
                "prime_usdc_liquidity_supply",
                self.prime_usdc_reserve.reserve_liquidity_supply,
            ),
            ("prime_usdc_reserve", self.prime_usdc_reserve.reserve),
            (
                "prime_usdc_collateral_mint",
                self.prime_usdc_reserve.collateral_mint,
            ),
            (
                "pyusd_liquidity_supply",
                self.pyusd_reserve.reserve_liquidity_supply,
            ),
            ("pyusd_reserve", self.pyusd_reserve.reserve),
            ("pyusd_collateral_mint", self.pyusd_reserve.collateral_mint),
        ]
    }

    fn custody_snapshot(&self) -> CustodySnapshot {
        self.custody_snapshot_with_extra(&[])
    }

    fn custody_snapshot_with_extra(&self, extra_accounts: &[(&str, Pubkey)]) -> CustodySnapshot {
        let mut snapshots = self
            .custody_accounts()
            .into_iter()
            .map(|(label, pubkey)| self.snapshot_account(label, pubkey))
            .collect::<Vec<_>>();
        snapshots.extend(
            extra_accounts
                .iter()
                .map(|(label, pubkey)| self.snapshot_account(label, *pubkey)),
        );
        CustodySnapshot(snapshots)
    }

    fn snapshot_account(&self, label: &str, pubkey: Pubkey) -> CustodyAccountSnapshot {
        let account = self.context.svm.get_account(&pubkey);
        let token_amount = account.as_ref().and_then(|account| {
            if account.owner == spl_token::id() {
                spl_token::state::Account::unpack(&account.data)
                    .ok()
                    .map(|token| token.amount)
            } else {
                None
            }
        });
        CustodyAccountSnapshot {
            label: label.to_string(),
            pubkey,
            owner: account.as_ref().map(|account| account.owner),
            lamports: account.as_ref().map(|account| account.lamports),
            data_len: account.as_ref().map(|account| account.data.len()),
            token_amount,
        }
    }

    fn assert_custody_unchanged(&self, before: &CustodySnapshot) {
        let after = self.custody_snapshot();
        assert_eq!(&after, before, "rejected route changed custody state");
    }

    fn assert_custody_unchanged_with_extra(
        &self,
        before: &CustodySnapshot,
        extra_accounts: &[(&str, Pubkey)],
    ) {
        let after = self.custody_snapshot_with_extra(extra_accounts);
        assert_eq!(&after, before, "rejected route changed custody state");
    }

    fn raw_same_mint_route(&self) -> RawRoutePayload {
        let (withdraw_instructions, withdraw_accounts) = self.main_withdraw();
        let (deposit_instructions, deposit_accounts) = self.prime_deposit(AMOUNT);
        raw_route_from_parts(
            vec![
                (withdraw_instructions, withdraw_accounts),
                (deposit_instructions, deposit_accounts),
            ],
            vec![0, 3],
        )
    }

    fn raw_jupiter_route(&self) -> RawRoutePayload {
        let (withdraw_instructions, withdraw_accounts) = self.main_withdraw();
        let (deposit_instructions, deposit_accounts) = self.pyusd_deposit(AMOUNT);
        raw_route_from_parts(
            vec![
                (withdraw_instructions, withdraw_accounts),
                raw_jupiter_swap_part(
                    self.context.vault,
                    self.vault_usdc,
                    self.vault_pyusd,
                    USDC_MINT,
                    PYUSD_MINT,
                    JUPITER_V6_PROGRAM_ID,
                ),
                (deposit_instructions, deposit_accounts),
            ],
            vec![0, 1, 3],
        )
    }

    fn raw_hub_route(&self) -> RawRoutePayload {
        let (withdraw_instructions, withdraw_accounts) = self.main_withdraw();
        let (deposit_instructions, deposit_accounts) = self.pyusd_deposit(HUB_OUT);
        raw_route_from_parts(
            vec![
                (withdraw_instructions, withdraw_accounts),
                raw_hub_swap_part(hub_swap(
                    self,
                    self.vault_usdc,
                    self.vault_pyusd,
                    USDC_MINT,
                    PYUSD_MINT,
                )),
                (deposit_instructions, deposit_accounts),
            ],
            vec![0, 2, 3],
        )
    }

    fn send_raw_route(
        &mut self,
        payload: RawRoutePayload,
        include_hub_authorizer: bool,
    ) -> Result<(), String> {
        let ix = execute_squads_program_interaction_instruction(
            self.route_action.accounts.withdraw,
            self.wallet_b.pubkey(),
            self.context.vault_index,
            payload.compiled_instructions,
            payload.instruction_constraint_indices,
            payload.transaction_accounts,
        );
        let extra_signers = if include_hub_authorizer {
            vec![&self.hub_authorizer]
        } else {
            Vec::new()
        };
        std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            try_send_instructions_with_heap_frame(
                &mut self.context.svm,
                &[ix],
                &self.wallet_b,
                &extra_signers,
            )
        }))
        .unwrap_or_else(|panic| {
            let message = panic
                .downcast_ref::<String>()
                .cloned()
                .or_else(|| {
                    panic
                        .downcast_ref::<&str>()
                        .map(|message| message.to_string())
                })
                .unwrap_or_else(|| "transaction construction panicked".to_string());
            Err(message)
        })
    }
}

fn setup_fixture() -> Option<AdversarialRouteFixture> {
    let fixture = AdversarialRouteFixture::setup();
    if fixture.is_none() {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
    }
    fixture
}

#[test]
fn happy_path_same_mint_jupiter_and_hub_routes_are_live() {
    let Some(mut same_mint) = setup_fixture() else {
        return;
    };
    let (withdraw_instructions, withdraw_accounts) = same_mint.main_withdraw();
    let (deposit_instructions, deposit_accounts) = same_mint.prime_deposit(AMOUNT);
    let same_mint_ix = same_mint
        .route_action
        .same_mint_route_action()
        .expect("same-mint route")
        .build(
            same_mint.wallet_b.pubkey(),
            same_mint.context.vault_index,
            withdraw_instructions,
            withdraw_accounts,
            deposit_instructions,
            deposit_accounts,
        );
    try_send_instructions_with_heap_frame(
        &mut same_mint.context.svm,
        &[same_mint_ix],
        &same_mint.wallet_b,
        &[],
    )
    .expect("valid same-mint route succeeds");
    assert_eq!(
        get_spl_token_amount(
            &same_mint.context.svm,
            same_mint.vault_prime_usdc_collateral
        ),
        AMOUNT
    );

    let Some(mut jupiter) = setup_fixture() else {
        return;
    };
    let (withdraw_instructions, withdraw_accounts) = jupiter.main_withdraw();
    let (deposit_instructions, deposit_accounts) = jupiter.pyusd_deposit(AMOUNT);
    let jupiter_ix = jupiter
        .route_action
        .jupiter_route_action()
        .expect("Jupiter route")
        .build(JupiterRouteExecution {
            withdraw_instructions,
            withdraw_accounts,
            swap: JupiterSwapExecution {
                signer: jupiter.wallet_b.pubkey(),
                vault_index: jupiter.context.vault_index,
                vault: jupiter.context.vault,
                vault_input: jupiter.vault_usdc,
                vault_output: jupiter.vault_pyusd,
                input_mint: USDC_MINT,
                output_mint: PYUSD_MINT,
                in_amount: AMOUNT,
                out_amount: AMOUNT,
            },
            deposit_instructions,
            deposit_accounts,
        });
    try_send_instructions_with_heap_frame(
        &mut jupiter.context.svm,
        &[jupiter_ix],
        &jupiter.wallet_b,
        &[],
    )
    .expect("valid Jupiter cross-mint route succeeds");
    assert_eq!(
        get_spl_token_amount(&jupiter.context.svm, jupiter.vault_pyusd_collateral),
        AMOUNT
    );

    let Some(mut hub) = setup_fixture() else {
        return;
    };
    let (withdraw_instructions, withdraw_accounts) = hub.main_withdraw();
    let (deposit_instructions, deposit_accounts) = hub.pyusd_deposit(HUB_OUT);
    let hub_ix = hub
        .route_action
        .loyal_hub_route_action()
        .expect("Loyal Hub route")
        .build(HubRouteExecution {
            withdraw_instructions,
            withdraw_accounts,
            swap: hub_swap(&hub, hub.vault_usdc, hub.vault_pyusd, USDC_MINT, PYUSD_MINT),
            deposit_instructions,
            deposit_accounts,
        });
    try_send_instructions_with_heap_frame(
        &mut hub.context.svm,
        &[hub_ix],
        &hub.wallet_b,
        &[&hub.hub_authorizer],
    )
    .expect("valid Loyal Hub cross-mint route succeeds");
    assert_eq!(
        get_spl_token_amount(&hub.context.svm, hub.vault_pyusd_collateral),
        HUB_OUT
    );
}

#[test]
fn raw_route_builders_match_sdk_semantics_before_mutation() {
    let Some(mut same_mint) = setup_fixture() else {
        return;
    };
    let payload = same_mint.raw_same_mint_route();
    let result = same_mint.send_raw_route(payload, false);
    assert!(result.is_ok(), "valid raw same-mint route should succeed");
    assert_eq!(
        get_spl_token_amount(
            &same_mint.context.svm,
            same_mint.vault_prime_usdc_collateral
        ),
        AMOUNT
    );

    let Some(mut jupiter) = setup_fixture() else {
        return;
    };
    let payload = jupiter.raw_jupiter_route();
    let result = jupiter.send_raw_route(payload, false);
    assert!(result.is_ok(), "valid raw Jupiter route should succeed");
    assert_eq!(
        get_spl_token_amount(&jupiter.context.svm, jupiter.vault_pyusd_collateral),
        AMOUNT
    );

    let Some(mut hub) = setup_fixture() else {
        return;
    };
    let payload = hub.raw_hub_route();
    let result = hub.send_raw_route(payload, true);
    assert!(result.is_ok(), "valid raw Hub route should succeed");
    assert_eq!(
        get_spl_token_amount(&hub.context.svm, hub.vault_pyusd_collateral),
        HUB_OUT
    );
}

#[test]
fn adversarial_loyal_hub_program_can_drain_route_value_past_all_in_one_policy() {
    let Some(mut fixture) = AdversarialRouteFixture::setup_with_adversarial_hub_program() else {
        eprintln!("skipping real Squads policy test; set SQUADS_SMART_ACCOUNT_PROGRAM_SO");
        return;
    };

    let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
    let (deposit_instructions, deposit_accounts) = fixture.pyusd_deposit(ADVERSARIAL_HUB_DUST_OUT);
    let route_ix = fixture
        .route_action
        .loyal_hub_route_action()
        .expect("Loyal Hub route")
        .build(HubRouteExecution {
            withdraw_instructions,
            withdraw_accounts,
            swap: HubSwapExecution {
                amount_out: ADVERSARIAL_HUB_DUST_OUT,
                min_out: ADVERSARIAL_HUB_DUST_OUT,
                ..hub_swap(
                    &fixture,
                    fixture.vault_usdc,
                    fixture.vault_pyusd,
                    USDC_MINT,
                    PYUSD_MINT,
                )
            },
            deposit_instructions,
            deposit_accounts,
        });

    try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[route_ix],
        &fixture.wallet_b,
        &[&fixture.hub_authorizer],
    )
    .expect("all-in-one policy accepts a policy-shaped adversarial Hub route");

    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_main_usdc_collateral),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_usdc),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd),
        0
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, fixture.vault_pyusd_collateral),
        ADVERSARIAL_HUB_DUST_OUT
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, loyal_hub_token_account(USDC_MINT)),
        AMOUNT
    );
}

#[test]
fn rejects_kamino_noncanonical_supply_or_non_vault_destinations() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    for destination in [
        fixture.attacker_prime_usdc_collateral,
        fixture.second_vault_prime_usdc_collateral,
    ] {
        let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
        let (deposit_instructions, mut deposit_accounts) = fixture.prime_deposit(AMOUNT);
        deposit_accounts[8] = AccountMeta::new(destination, false);
        let route_ix = fixture
            .route_action
            .same_mint_route_action()
            .expect("same-mint route")
            .build(
                fixture.wallet_b.pubkey(),
                fixture.context.vault_index,
                withdraw_instructions,
                withdraw_accounts,
                deposit_instructions,
                deposit_accounts,
            );
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[route_ix],
            &fixture.wallet_b,
            &[],
        );
        assert!(
            result.is_err(),
            "K-Lend should reject a noncanonical reserve collateral supply"
        );
        assert_eq!(get_spl_token_amount(&fixture.context.svm, destination), 0);
        fixture.assert_starting_balances();
    }

    for destination in [fixture.attacker_usdc, fixture.second_vault_usdc] {
        let (withdraw_instructions, mut withdraw_accounts) = fixture.main_withdraw();
        withdraw_accounts[9] = AccountMeta::new(destination, false);
        let withdraw_ix = fixture
            .route_action
            .withdraw()
            .expect("withdraw step")
            .build(
                fixture.wallet_b.pubkey(),
                fixture.context.vault_index,
                withdraw_instructions,
                withdraw_accounts,
            );
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[withdraw_ix],
            &fixture.wallet_b,
            &[],
        );
        assert!(
            result.is_err(),
            "policy should reject non-vault liquidity destination"
        );
        assert_eq!(
            get_spl_token_amount(&fixture.context.svm, destination),
            AMOUNT
        );
        fixture.assert_starting_balances();
    }
}

#[test]
fn rejects_swap_output_accounts_not_owned_by_this_vault() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    for output in [fixture.attacker_pyusd, fixture.second_vault_pyusd] {
        let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
        let (deposit_instructions, deposit_accounts) = fixture.pyusd_deposit(AMOUNT);
        let route_ix = fixture
            .route_action
            .jupiter_route_action()
            .expect("Jupiter route")
            .build(JupiterRouteExecution {
                withdraw_instructions,
                withdraw_accounts,
                swap: JupiterSwapExecution {
                    signer: fixture.wallet_b.pubkey(),
                    vault_index: fixture.context.vault_index,
                    vault: fixture.context.vault,
                    vault_input: fixture.vault_usdc,
                    vault_output: output,
                    input_mint: USDC_MINT,
                    output_mint: PYUSD_MINT,
                    in_amount: AMOUNT,
                    out_amount: AMOUNT,
                },
                deposit_instructions,
                deposit_accounts,
            });
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[route_ix],
            &fixture.wallet_b,
            &[],
        );
        assert!(
            result.is_err(),
            "policy should reject non-vault Jupiter output"
        );
        assert_eq!(get_spl_token_amount(&fixture.context.svm, output), 0);
        fixture.assert_starting_balances();
    }

    for output in [fixture.attacker_pyusd, fixture.second_vault_pyusd] {
        let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
        let (deposit_instructions, deposit_accounts) = fixture.pyusd_deposit(HUB_OUT);
        let route_ix = fixture
            .route_action
            .loyal_hub_route_action()
            .expect("Loyal Hub route")
            .build(HubRouteExecution {
                withdraw_instructions,
                withdraw_accounts,
                swap: hub_swap(&fixture, fixture.vault_usdc, output, USDC_MINT, PYUSD_MINT),
                deposit_instructions,
                deposit_accounts,
            });
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[route_ix],
            &fixture.wallet_b,
            &[&fixture.hub_authorizer],
        );
        assert!(
            result.is_err(),
            "policy or hub should reject non-vault Loyal Hub output"
        );
        assert_eq!(get_spl_token_amount(&fixture.context.svm, output), 0);
        fixture.assert_starting_balances();
    }
}

#[test]
fn substituted_source_accounts_cannot_move_vault_funds_or_use_vault_as_signer_oracle() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    for source in [fixture.attacker_usdc, fixture.second_vault_usdc] {
        let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
        let (deposit_instructions, mut deposit_accounts) = fixture.prime_deposit(AMOUNT);
        deposit_accounts[7] = AccountMeta::new(source, false);
        let route_ix = fixture
            .route_action
            .same_mint_route_action()
            .expect("same-mint route")
            .build(
                fixture.wallet_b.pubkey(),
                fixture.context.vault_index,
                withdraw_instructions,
                withdraw_accounts,
                deposit_instructions,
                deposit_accounts,
            );
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[route_ix],
            &fixture.wallet_b,
            &[],
        );
        assert!(result.is_err(), "substituted deposit source should fail");
        assert_eq!(get_spl_token_amount(&fixture.context.svm, source), AMOUNT);
        fixture.assert_starting_balances();
    }

    for source in [
        fixture.attacker_main_usdc_collateral,
        fixture.second_vault_main_usdc_collateral,
    ] {
        let (withdraw_instructions, mut withdraw_accounts) = fixture.main_withdraw();
        withdraw_accounts[7] = AccountMeta::new(source, false);
        let withdraw_ix = fixture
            .route_action
            .withdraw()
            .expect("withdraw step")
            .build(
                fixture.wallet_b.pubkey(),
                fixture.context.vault_index,
                withdraw_instructions,
                withdraw_accounts,
            );
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[withdraw_ix],
            &fixture.wallet_b,
            &[],
        );
        assert!(result.is_err(), "substituted withdraw source should fail");
        assert_eq!(get_spl_token_amount(&fixture.context.svm, source), AMOUNT);
        fixture.assert_starting_balances();
    }

    for source in [fixture.attacker_usdc, fixture.second_vault_usdc] {
        let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
        let (deposit_instructions, deposit_accounts) = fixture.pyusd_deposit(AMOUNT);
        let route_ix = fixture
            .route_action
            .jupiter_route_action()
            .expect("Jupiter route")
            .build(JupiterRouteExecution {
                withdraw_instructions,
                withdraw_accounts,
                swap: JupiterSwapExecution {
                    signer: fixture.wallet_b.pubkey(),
                    vault_index: fixture.context.vault_index,
                    vault: fixture.context.vault,
                    vault_input: source,
                    vault_output: fixture.vault_pyusd,
                    input_mint: USDC_MINT,
                    output_mint: PYUSD_MINT,
                    in_amount: AMOUNT,
                    out_amount: AMOUNT,
                },
                deposit_instructions,
                deposit_accounts,
            });
        let result = try_send_instructions_with_heap_frame(
            &mut fixture.context.svm,
            &[route_ix],
            &fixture.wallet_b,
            &[],
        );
        assert!(result.is_err(), "substituted Jupiter input should fail");
        assert_eq!(get_spl_token_amount(&fixture.context.svm, source), AMOUNT);
        fixture.assert_starting_balances();
    }
}

#[test]
fn rejects_unapproved_mints_markets_and_venue_accounts() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let unapproved_mint = Pubkey::new_unique();
    let unapproved_reserve = Pubkey::new_unique();
    let unapproved_vault_liquidity = Keypair::new().pubkey();
    let unapproved_vault_collateral = Keypair::new().pubkey();
    let unapproved_supply = Keypair::new().pubkey();
    seed_spl_mint_if_missing(
        &mut fixture.context.svm,
        unapproved_mint,
        None,
        USDC_DECIMALS,
        0,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        unapproved_vault_liquidity,
        unapproved_mint,
        fixture.context.vault,
        AMOUNT,
    );
    let unapproved_reserve_accounts = seed_mock_kamino_reserve_spl_accounts_with_mint(
        &mut fixture.context.svm,
        unapproved_reserve,
        KAMINO_MAIN_MARKET,
        unapproved_mint,
        USDC_DECIMALS,
        fixture.context.vault,
        unapproved_vault_liquidity,
        unapproved_vault_collateral,
        unapproved_supply,
    );
    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        fixture.context.vault,
        unapproved_reserve_accounts,
        mock_kamino_deposit_reserve_liquidity_data(AMOUNT),
    );
    let deposit_ix = fixture.route_action.deposit().expect("deposit step").build(
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        deposit_instructions,
        deposit_accounts,
    );
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[deposit_ix],
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "approved market with unapproved liquidity mint should fail"
    );
    assert_eq!(
        get_spl_token_amount(&fixture.context.svm, unapproved_vault_collateral),
        0
    );

    let bad_output_mint = Pubkey::new_unique();
    let bad_output = Keypair::new().pubkey();
    seed_spl_mint_if_missing(
        &mut fixture.context.svm,
        bad_output_mint,
        None,
        PYUSD_DECIMALS,
        0,
    );
    seed_spl_token_account(
        &mut fixture.context.svm,
        bad_output,
        bad_output_mint,
        fixture.context.vault,
        0,
    );
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut fixture.context.svm,
        &[MockJupiterStableReserveTokenAccount {
            mint: bad_output_mint,
            reserve: mock_jupiter_stable_reserve_token_account(bad_output_mint),
        }],
        AMOUNT,
    );
    let swap_ix =
        fixture
            .route_action
            .jupiter()
            .expect("Jupiter step")
            .build(JupiterSwapExecution {
                signer: fixture.wallet_b.pubkey(),
                vault_index: fixture.context.vault_index,
                vault: fixture.context.vault,
                vault_input: fixture.vault_usdc,
                vault_output: bad_output,
                input_mint: USDC_MINT,
                output_mint: bad_output_mint,
                in_amount: AMOUNT,
                out_amount: AMOUNT,
            });
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[swap_ix],
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "unapproved Jupiter output mint should fail"
    );
    assert_eq!(get_spl_token_amount(&fixture.context.svm, bad_output), 0);

    let wrong_jupiter_data_ix = execute_squads_program_interaction_instruction(
        fixture.route_action.accounts.swap,
        fixture.wallet_b.pubkey(),
        fixture.context.vault_index,
        vec![SquadsCompiledInstruction {
            program_id_index: 9,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8],
            data: mock_jupiter_swap_data(MOCK_JUPITER_USDC_TO_PYUSD, AMOUNT, USDC_MINT, PYUSD_MINT),
        }],
        vec![1],
        jupiter_transaction_accounts(
            fixture.context.vault,
            fixture.vault_usdc,
            fixture.vault_pyusd,
            USDC_MINT,
            PYUSD_MINT,
            JUPITER_V6_PROGRAM_ID,
        ),
    );
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[wrong_jupiter_data_ix],
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "wrong Jupiter discriminator should fail at policy"
    );

    let before = fixture.custody_snapshot();
    let mut excessive_slippage_payload = fixture.raw_jupiter_route();
    excessive_slippage_payload.compiled_instructions[1].data =
        mock_jupiter_stable_exact_in_swap_data_with_slippage(
            AMOUNT, AMOUNT, 101, USDC_MINT, PYUSD_MINT,
        );
    let result = fixture.send_raw_route(excessive_slippage_payload, false);
    assert!(
        result.is_err(),
        "Jupiter slippage above the policy cap should fail"
    );
    fixture.assert_custody_unchanged(&before);

    let wrong_hub_authorizer = Keypair::new();
    let wrong_hub_authorizer_ix =
        fixture
            .route_action
            .hub()
            .expect("Hub step")
            .build(HubSwapExecution {
                hub_authorizer: wrong_hub_authorizer.pubkey(),
                ..hub_swap(
                    &fixture,
                    fixture.vault_usdc,
                    fixture.vault_pyusd,
                    USDC_MINT,
                    PYUSD_MINT,
                )
            });
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[wrong_hub_authorizer_ix],
        &fixture.wallet_b,
        &[&wrong_hub_authorizer],
    );
    assert!(result.is_err(), "wrong Loyal Hub authorizer should fail");
}

#[test]
fn rejects_route_shape_index_authority_lifecycle_and_duplicate_account_confusion() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let (deposit_instructions, deposit_accounts) = fixture.prime_deposit(AMOUNT);
    let deposit_with_withdraw_index = fixture
        .route_action
        .withdraw()
        .expect("withdraw step")
        .build(
            fixture.wallet_b.pubkey(),
            fixture.context.vault_index,
            deposit_instructions,
            deposit_accounts,
        );
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[deposit_with_withdraw_index],
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "deposit instruction using withdraw index should fail"
    );
    fixture.assert_starting_balances();

    let wrong_signer = Keypair::new();
    fixture
        .context
        .svm
        .airdrop(&wrong_signer.pubkey(), LAMPORTS_PER_SOL / 10)
        .expect("airdrop wrong signer");
    let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
    let withdraw_ix = fixture
        .route_action
        .withdraw()
        .expect("withdraw step")
        .build(
            wrong_signer.pubkey(),
            fixture.context.vault_index,
            withdraw_instructions,
            withdraw_accounts,
        );
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[withdraw_ix],
        &wrong_signer,
        &[],
    );
    assert!(result.is_err(), "wrong delegated signer should fail");
    fixture.assert_starting_balances();

    let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
    let wrong_vault_index_ix = fixture
        .route_action
        .withdraw()
        .expect("withdraw step")
        .build(
            fixture.wallet_b.pubkey(),
            fixture.context.vault_index + 1,
            withdraw_instructions,
            withdraw_accounts,
        );
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[wrong_vault_index_ix],
        &fixture.wallet_b,
        &[],
    );
    assert!(result.is_err(), "wrong vault index should fail");
    fixture.assert_starting_balances();

    let duplicate_swap_ix =
        fixture
            .route_action
            .jupiter()
            .expect("Jupiter step")
            .build(JupiterSwapExecution {
                signer: fixture.wallet_b.pubkey(),
                vault_index: fixture.context.vault_index,
                vault: fixture.context.vault,
                vault_input: fixture.vault_usdc,
                vault_output: fixture.vault_usdc,
                input_mint: USDC_MINT,
                output_mint: PYUSD_MINT,
                in_amount: AMOUNT,
                out_amount: AMOUNT,
            });
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[duplicate_swap_ix],
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "duplicate input/output token account should fail"
    );
    fixture.assert_starting_balances();

    let remove_ix = remove_squads_policy_instruction(
        fixture.context.pool.settings,
        fixture.context.wallet_pubkey(),
        fixture.route_action.accounts.withdraw,
    );
    try_send_instructions(
        &mut fixture.context.svm,
        &[remove_ix],
        &fixture.context.wallet,
        &[],
    )
    .expect("wallet A removes all-in-one policy");

    let (withdraw_instructions, withdraw_accounts) = fixture.main_withdraw();
    let post_removal_ix = fixture
        .route_action
        .withdraw()
        .expect("withdraw step")
        .build(
            fixture.wallet_b.pubkey(),
            fixture.context.vault_index,
            withdraw_instructions,
            withdraw_accounts,
        );
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &[post_removal_ix],
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "removed all-in-one policy should not execute"
    );
    fixture.assert_starting_balances();
}

#[test]
fn wallet_b_cannot_create_the_route_policy_for_itself() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };
    let wallet_b_created_action = create_all_in_one_market_mint_yield_route_action(
        LoyalActionContext {
            settings: fixture.context.pool.settings,
            authority: fixture.wallet_b.pubkey(),
            delegated_signer: fixture.wallet_b.pubkey(),
            account_index: fixture.context.vault_index,
            vault: fixture.context.vault,
        },
        yield_route_universe_from_mock_reserves(
            vec![USDC_MINT, PYUSD_MINT],
            vec![
                fixture.main_usdc_reserve,
                fixture.prime_usdc_reserve,
                fixture.pyusd_reserve,
            ],
        ),
        vec![
            mock_jupiter_swap_lane(false),
            SwapLane::LoyalHub {
                hub_authorizer: fixture.hub_authorizer.pubkey(),
                max_fee_bps: MAX_FEE_BPS,
            },
        ],
    )
    .expect("build Wallet B-authored route policy instruction");
    let result = try_send_instructions_with_heap_frame(
        &mut fixture.context.svm,
        &wallet_b_created_action.instructions,
        &fixture.wallet_b,
        &[],
    );
    assert!(
        result.is_err(),
        "Wallet B should not satisfy Wallet A's policy authority signer"
    );
}

#[test]
fn raw_route_payload_mutations_reject_and_roll_back_all_token_balances() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let cases: Vec<MutationCase> = vec![
        MutationCase {
            name: "withdraw program id points at Jupiter",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                let jupiter_index = push_or_update_account_meta(
                    &mut payload.transaction_accounts,
                    AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
                );
                payload.compiled_instructions[0].program_id_index = jupiter_index;
            },
            include_hub_authorizer: false,
            expected_boundary: "program-id constraint",
        },
        MutationCase {
            name: "swap program id points at Kamino",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                let kamino_index = push_or_update_account_meta(
                    &mut payload.transaction_accounts,
                    AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
                );
                payload.compiled_instructions[1].program_id_index = kamino_index;
            },
            include_hub_authorizer: false,
            expected_boundary: "program-id constraint",
        },
        MutationCase {
            name: "deposit program id points at Jupiter",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                let jupiter_index = push_or_update_account_meta(
                    &mut payload.transaction_accounts,
                    AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
                );
                payload.compiled_instructions[2].program_id_index = jupiter_index;
            },
            include_hub_authorizer: false,
            expected_boundary: "program-id constraint",
        },
        MutationCase {
            name: "Jupiter instruction uses Hub constraint index",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                payload.instruction_constraint_indices = vec![0, 2, 3];
            },
            include_hub_authorizer: false,
            expected_boundary: "constraint-index lane boundary",
        },
        MutationCase {
            name: "Hub instruction uses Jupiter constraint index",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            mutate: |_, payload| {
                payload.instruction_constraint_indices = vec![0, 1, 3];
            },
            include_hub_authorizer: true,
            expected_boundary: "constraint-index lane boundary",
        },
        MutationCase {
            name: "route is missing withdraw",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                payload.compiled_instructions.remove(0);
                payload.instruction_constraint_indices.remove(0);
            },
            include_hub_authorizer: false,
            expected_boundary: "route shape",
        },
        MutationCase {
            name: "route order is swap withdraw deposit",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                payload.compiled_instructions.swap(0, 1);
            },
            include_hub_authorizer: false,
            expected_boundary: "route order",
        },
        MutationCase {
            name: "route order is withdraw deposit swap",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                payload.compiled_instructions.swap(1, 2);
            },
            include_hub_authorizer: false,
            expected_boundary: "route order",
        },
        MutationCase {
            name: "valid route plus extra withdraw",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            mutate: |_, payload| {
                payload
                    .compiled_instructions
                    .push(copy_compiled_instruction(&payload.compiled_instructions[0]));
                payload.instruction_constraint_indices.push(0);
            },
            include_hub_authorizer: false,
            expected_boundary: "extra inner instruction",
        },
        MutationCase {
            name: "withdraw withdraw deposit",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            mutate: |_, payload| {
                payload.compiled_instructions.insert(
                    1,
                    copy_compiled_instruction(&payload.compiled_instructions[0]),
                );
                payload.instruction_constraint_indices.insert(1, 0);
            },
            include_hub_authorizer: false,
            expected_boundary: "duplicate route step",
        },
        MutationCase {
            name: "withdraw swap swap deposit",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                payload.compiled_instructions.insert(
                    2,
                    copy_compiled_instruction(&payload.compiled_instructions[1]),
                );
                payload.instruction_constraint_indices.insert(2, 1);
            },
            include_hub_authorizer: false,
            expected_boundary: "duplicate route step",
        },
    ];

    for case in cases {
        let mut payload = (case.payload_builder)(&fixture);
        (case.mutate)(&fixture, &mut payload);
        let before = fixture.custody_snapshot();
        let result = fixture.send_raw_route(payload, case.include_hub_authorizer);
        assert!(
            result.is_err(),
            "{} should reject at {}",
            case.name,
            case.expected_boundary
        );
        fixture.assert_custody_unchanged(&before);
    }
}

#[test]
fn raw_account_metadata_mutations_reject_without_custody_change() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let cases = vec![
        MutationCase {
            name: "same-mint vault marked as signer",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            mutate: |_, payload| flip_compiled_account_signer(payload, 0, 0),
            include_hub_authorizer: false,
            expected_boundary: "account signer metadata",
        },
        MutationCase {
            name: "same-mint withdraw destination not writable",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            mutate: |_, payload| flip_compiled_account_writable(payload, 0, 8),
            include_hub_authorizer: false,
            expected_boundary: "account writable metadata",
        },
        MutationCase {
            name: "same-mint deposit destination not writable",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            mutate: |_, payload| flip_compiled_account_writable(payload, 1, 8),
            include_hub_authorizer: false,
            expected_boundary: "account writable metadata",
        },
        MutationCase {
            name: "Jupiter input token account not writable",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| flip_compiled_account_writable(payload, 1, 1),
            include_hub_authorizer: false,
            expected_boundary: "account writable metadata",
        },
        MutationCase {
            name: "Jupiter token program marked as signer",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| flip_compiled_account_signer(payload, 1, 5),
            include_hub_authorizer: false,
            expected_boundary: "token program signer metadata",
        },
        MutationCase {
            name: "Jupiter program marked as signer",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| flip_compiled_program_signer(payload, 1),
            include_hub_authorizer: false,
            expected_boundary: "program signer metadata",
        },
        MutationCase {
            name: "Hub user vault marked as signer",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            mutate: |_, payload| flip_compiled_account_signer(payload, 1, 1),
            include_hub_authorizer: true,
            expected_boundary: "delegated signer metadata",
        },
        MutationCase {
            name: "Hub authorizer signer removed",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            mutate: |_, payload| flip_compiled_account_signer(payload, 1, 9),
            include_hub_authorizer: true,
            expected_boundary: "Hub authorizer signer metadata",
        },
        MutationCase {
            name: "Hub token program marked as signer",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            mutate: |_, payload| flip_compiled_account_signer(payload, 1, 10),
            include_hub_authorizer: true,
            expected_boundary: "token program signer metadata",
        },
    ];

    for case in cases {
        let mut payload = (case.payload_builder)(&fixture);
        (case.mutate)(&fixture, &mut payload);
        let before = fixture.custody_snapshot();
        let result = fixture.send_raw_route(payload, case.include_hub_authorizer);
        assert!(
            result.is_err(),
            "{} should reject at {}",
            case.name,
            case.expected_boundary
        );
        fixture.assert_custody_unchanged(&before);
    }
}

#[test]
fn raw_constrained_account_replacements_reject_without_custody_change() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let wrong_market = Pubkey::new_unique();
    let wrong_token_program = Pubkey::new_unique();
    let wrong_mint = Pubkey::new_unique();
    let wrong_config = Pubkey::new_unique();
    seed_empty_system_account_if_missing(&mut fixture.context.svm, wrong_market);
    seed_empty_system_account_if_missing(&mut fixture.context.svm, wrong_token_program);
    seed_empty_system_account_if_missing(&mut fixture.context.svm, wrong_config);
    seed_spl_mint_if_missing(&mut fixture.context.svm, wrong_mint, None, USDC_DECIMALS, 0);

    struct ReplacementCase {
        name: &'static str,
        payload_builder: fn(&AdversarialRouteFixture) -> RawRoutePayload,
        instruction_index: usize,
        account_position: usize,
        replacement: Pubkey,
        include_hub_authorizer: bool,
        expected_boundary: &'static str,
    }

    let cases = vec![
        ReplacementCase {
            name: "Kamino withdraw wrong vault",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 0,
            account_position: 0,
            replacement: fixture.second_vault,
            include_hub_authorizer: false,
            expected_boundary: "withdraw vault pubkey constraint",
        },
        ReplacementCase {
            name: "Kamino withdraw wrong market",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 0,
            account_position: 2,
            replacement: wrong_market,
            include_hub_authorizer: false,
            expected_boundary: "withdraw market pubkey constraint",
        },
        ReplacementCase {
            name: "Kamino withdraw wrong liquidity mint",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 0,
            account_position: 5,
            replacement: wrong_mint,
            include_hub_authorizer: false,
            expected_boundary: "withdraw liquidity mint constraint",
        },
        ReplacementCase {
            name: "Kamino withdraw wrong token owner",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 0,
            account_position: 9,
            replacement: fixture.attacker_usdc,
            include_hub_authorizer: false,
            expected_boundary: "withdraw destination owner constraint",
        },
        ReplacementCase {
            name: "Kamino withdraw wrong token program",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 0,
            account_position: 11,
            replacement: wrong_token_program,
            include_hub_authorizer: false,
            expected_boundary: "withdraw token program constraint",
        },
        ReplacementCase {
            name: "Kamino deposit wrong vault",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 1,
            account_position: 0,
            replacement: fixture.second_vault,
            include_hub_authorizer: false,
            expected_boundary: "deposit vault pubkey constraint",
        },
        ReplacementCase {
            name: "Kamino deposit wrong market",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 1,
            account_position: 2,
            replacement: wrong_market,
            include_hub_authorizer: false,
            expected_boundary: "deposit market pubkey constraint",
        },
        ReplacementCase {
            name: "Kamino deposit wrong liquidity mint",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 1,
            account_position: 5,
            replacement: wrong_mint,
            include_hub_authorizer: false,
            expected_boundary: "deposit liquidity mint constraint",
        },
        ReplacementCase {
            name: "Kamino deposit noncanonical collateral supply",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 1,
            account_position: 8,
            replacement: fixture.attacker_prime_usdc_collateral,
            include_hub_authorizer: false,
            expected_boundary: "K-Lend reserve collateral supply constraint",
        },
        ReplacementCase {
            name: "Kamino deposit wrong token program",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index: 1,
            account_position: 11,
            replacement: wrong_token_program,
            include_hub_authorizer: false,
            expected_boundary: "deposit token program constraint",
        },
        ReplacementCase {
            name: "Jupiter wrong vault",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            instruction_index: 1,
            account_position: 0,
            replacement: fixture.second_vault,
            include_hub_authorizer: false,
            expected_boundary: "Jupiter vault pubkey constraint",
        },
        ReplacementCase {
            name: "Jupiter wrong input token account",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            instruction_index: 1,
            account_position: 1,
            replacement: fixture.attacker_usdc,
            include_hub_authorizer: false,
            expected_boundary: "Jupiter input account data constraint",
        },
        ReplacementCase {
            name: "Jupiter wrong output token account",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            instruction_index: 1,
            account_position: 2,
            replacement: fixture.attacker_pyusd,
            include_hub_authorizer: false,
            expected_boundary: "Jupiter output owner constraint",
        },
        ReplacementCase {
            name: "Jupiter wrong input mint",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            instruction_index: 1,
            account_position: 3,
            replacement: wrong_mint,
            include_hub_authorizer: false,
            expected_boundary: "Jupiter input mint constraint",
        },
        ReplacementCase {
            name: "Jupiter wrong output mint",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            instruction_index: 1,
            account_position: 4,
            replacement: wrong_mint,
            include_hub_authorizer: false,
            expected_boundary: "Jupiter output mint constraint",
        },
        ReplacementCase {
            name: "Jupiter wrong token program",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            instruction_index: 1,
            account_position: 5,
            replacement: wrong_token_program,
            include_hub_authorizer: false,
            expected_boundary: "Jupiter token program constraint",
        },
        ReplacementCase {
            name: "Hub wrong config",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::CONFIG as usize,
            replacement: wrong_config,
            include_hub_authorizer: true,
            expected_boundary: "Hub config pubkey constraint",
        },
        ReplacementCase {
            name: "Hub wrong user vault",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::USER_VAULT as usize,
            replacement: fixture.second_vault,
            include_hub_authorizer: true,
            expected_boundary: "Hub vault pubkey constraint",
        },
        ReplacementCase {
            name: "Hub wrong input inventory",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::HUB_INPUT as usize,
            replacement: fixture.attacker_usdc,
            include_hub_authorizer: true,
            expected_boundary: "Hub input inventory account data constraint",
        },
        ReplacementCase {
            name: "Hub wrong output inventory",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::HUB_OUTPUT as usize,
            replacement: fixture.attacker_pyusd,
            include_hub_authorizer: true,
            expected_boundary: "Hub output inventory account data constraint",
        },
        ReplacementCase {
            name: "Hub wrong user output",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::USER_OUTPUT as usize,
            replacement: fixture.attacker_pyusd,
            include_hub_authorizer: true,
            expected_boundary: "Hub user output owner constraint",
        },
        ReplacementCase {
            name: "Hub wrong input mint",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::INPUT_MINT as usize,
            replacement: wrong_mint,
            include_hub_authorizer: true,
            expected_boundary: "Hub input mint constraint",
        },
        ReplacementCase {
            name: "Hub wrong output mint",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::OUTPUT_MINT as usize,
            replacement: wrong_mint,
            include_hub_authorizer: true,
            expected_boundary: "Hub output mint constraint",
        },
        ReplacementCase {
            name: "Hub wrong authorizer",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER
                as usize,
            replacement: fixture.wallet_b.pubkey(),
            include_hub_authorizer: true,
            expected_boundary: "Hub authorizer constraint",
        },
        ReplacementCase {
            name: "Hub wrong token program",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position: loyal_actions::hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM
                as usize,
            replacement: wrong_token_program,
            include_hub_authorizer: true,
            expected_boundary: "Hub token program constraint",
        },
    ];

    for case in cases {
        let mut payload = (case.payload_builder)(&fixture);
        replace_compiled_account(
            &mut payload,
            case.instruction_index,
            case.account_position,
            case.replacement,
        );
        let before = fixture.custody_snapshot();
        let result = fixture.send_raw_route(payload, case.include_hub_authorizer);
        assert!(
            result.is_err(),
            "{} should reject at {}",
            case.name,
            case.expected_boundary
        );
        fixture.assert_custody_unchanged(&before);
    }
}

#[test]
fn raw_lane_shape_confusion_rejects_without_custody_change() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let cases = vec![
        MutationCase {
            name: "Jupiter program id with Hub-shaped accounts and data",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            mutate: |_, payload| {
                let program_index = push_or_update_account_meta(
                    &mut payload.transaction_accounts,
                    AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
                );
                payload.compiled_instructions[1].program_id_index = program_index;
                payload.instruction_constraint_indices = vec![0, 1, 3];
            },
            include_hub_authorizer: true,
            expected_boundary: "Jupiter lane shape",
        },
        MutationCase {
            name: "Hub program id with Jupiter-shaped accounts and data",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| {
                let program_index = push_or_update_account_meta(
                    &mut payload.transaction_accounts,
                    AccountMeta::new_readonly(LOYAL_HUB_SWAP_PROGRAM_ID, false),
                );
                payload.compiled_instructions[1].program_id_index = program_index;
                payload.instruction_constraint_indices = vec![0, 2, 3];
            },
            include_hub_authorizer: true,
            expected_boundary: "Hub lane shape",
        },
        MutationCase {
            name: "Hub-shaped instruction under Jupiter constraint",
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            mutate: |_, payload| payload.instruction_constraint_indices = vec![0, 1, 3],
            include_hub_authorizer: true,
            expected_boundary: "constraint-index lane boundary",
        },
        MutationCase {
            name: "Jupiter-shaped instruction under Hub constraint",
            payload_builder: AdversarialRouteFixture::raw_jupiter_route,
            mutate: |_, payload| payload.instruction_constraint_indices = vec![0, 2, 3],
            include_hub_authorizer: false,
            expected_boundary: "constraint-index lane boundary",
        },
        MutationCase {
            name: "valid raw route plus unrelated inner instruction",
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            mutate: |_, payload| {
                let system_index = push_or_update_account_meta(
                    &mut payload.transaction_accounts,
                    AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
                );
                payload
                    .compiled_instructions
                    .push(SquadsCompiledInstruction {
                        program_id_index: system_index,
                        accounts: vec![],
                        data: vec![],
                    });
                payload.instruction_constraint_indices.push(0);
            },
            include_hub_authorizer: false,
            expected_boundary: "extra unrelated instruction",
        },
    ];

    for case in cases {
        let mut payload = (case.payload_builder)(&fixture);
        (case.mutate)(&fixture, &mut payload);
        let before = fixture.custody_snapshot();
        let result = fixture.send_raw_route(payload, case.include_hub_authorizer);
        assert!(
            result.is_err(),
            "{} should reject at {}",
            case.name,
            case.expected_boundary
        );
        fixture.assert_custody_unchanged(&before);
    }
}

#[test]
fn raw_account_data_spoofs_reject_without_value_escape() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let system_account = Keypair::new().pubkey();
    seed_empty_system_account_if_missing(&mut fixture.context.svm, system_account);
    let short_token_account = Keypair::new().pubkey();
    fixture
        .context
        .svm
        .set_account(
            short_token_account,
            Account {
                lamports: LAMPORTS_PER_SOL,
                data: vec![0; 8],
                owner: spl_token::id(),
                executable: false,
                rent_epoch: 0,
            },
        )
        .expect("seed short token account spoof");
    let mint_as_token_account = Pubkey::new_unique();
    seed_spl_mint_if_missing(
        &mut fixture.context.svm,
        mint_as_token_account,
        None,
        PYUSD_DECIMALS,
        0,
    );

    let spoof_accounts = [
        (
            "withdraw_source_wrong_mint",
            fixture.main_usdc_reserve.collateral_mint,
            USDC_MINT,
            fixture.context.vault,
            AMOUNT,
        ),
        (
            "withdraw_source_wrong_owner",
            fixture.main_usdc_reserve.collateral_mint,
            fixture.main_usdc_reserve.collateral_mint,
            fixture.attacker,
            AMOUNT,
        ),
        (
            "withdraw_destination_wrong_mint",
            USDC_MINT,
            PYUSD_MINT,
            fixture.context.vault,
            0,
        ),
        (
            "withdraw_destination_wrong_owner",
            USDC_MINT,
            USDC_MINT,
            fixture.attacker,
            0,
        ),
        (
            "deposit_source_wrong_mint",
            USDC_MINT,
            PYUSD_MINT,
            fixture.context.vault,
            AMOUNT,
        ),
        (
            "deposit_source_wrong_owner",
            USDC_MINT,
            USDC_MINT,
            fixture.attacker,
            AMOUNT,
        ),
        (
            "deposit_destination_wrong_mint",
            fixture.prime_usdc_reserve.collateral_mint,
            USDC_MINT,
            fixture.context.vault,
            0,
        ),
        (
            "deposit_destination_wrong_owner",
            fixture.prime_usdc_reserve.collateral_mint,
            fixture.prime_usdc_reserve.collateral_mint,
            fixture.attacker,
            0,
        ),
        (
            "jupiter_input_wrong_mint",
            USDC_MINT,
            PYUSD_MINT,
            fixture.context.vault,
            AMOUNT,
        ),
        (
            "jupiter_input_wrong_owner",
            USDC_MINT,
            USDC_MINT,
            fixture.attacker,
            AMOUNT,
        ),
        (
            "jupiter_output_wrong_mint",
            PYUSD_MINT,
            USDC_MINT,
            fixture.context.vault,
            0,
        ),
        (
            "jupiter_output_wrong_owner",
            PYUSD_MINT,
            PYUSD_MINT,
            fixture.attacker,
            0,
        ),
        (
            "hub_user_input_wrong_mint",
            USDC_MINT,
            PYUSD_MINT,
            fixture.context.vault,
            AMOUNT,
        ),
        (
            "hub_user_input_wrong_owner",
            USDC_MINT,
            USDC_MINT,
            fixture.attacker,
            AMOUNT,
        ),
        (
            "hub_user_output_wrong_mint",
            PYUSD_MINT,
            USDC_MINT,
            fixture.context.vault,
            0,
        ),
        (
            "hub_user_output_wrong_owner",
            PYUSD_MINT,
            PYUSD_MINT,
            fixture.attacker,
            0,
        ),
        (
            "hub_wrong_input_inventory",
            USDC_MINT,
            USDC_MINT,
            derive_loyal_hub_authority(),
            10 * AMOUNT,
        ),
        (
            "hub_wrong_output_inventory",
            PYUSD_MINT,
            PYUSD_MINT,
            derive_loyal_hub_authority(),
            10 * AMOUNT,
        ),
    ];
    let spoof_pubkeys = spoof_accounts
        .iter()
        .map(|_| Keypair::new().pubkey())
        .collect::<Vec<_>>();
    for ((_, _, mint, owner, amount), pubkey) in spoof_accounts.iter().zip(spoof_pubkeys.iter()) {
        seed_spl_token_account(&mut fixture.context.svm, *pubkey, *mint, *owner, *amount);
    }
    let mut extra_accounts = vec![
        ("spoof_system_account", system_account),
        ("spoof_short_token_account", short_token_account),
        ("spoof_mint_as_token_account", mint_as_token_account),
    ];
    extra_accounts.extend(
        spoof_accounts
            .iter()
            .zip(spoof_pubkeys.iter())
            .map(|((label, _, _, _, _), pubkey)| (*label, *pubkey)),
    );
    let spoof = |label: &str| {
        spoof_accounts
            .iter()
            .zip(spoof_pubkeys.iter())
            .find_map(|((candidate, _, _, _, _), pubkey)| (*candidate == label).then_some(*pubkey))
            .expect("spoof account exists")
    };

    struct SpoofCase {
        name: &'static str,
        payload_builder: fn(&AdversarialRouteFixture) -> RawRoutePayload,
        instruction_index: usize,
        account_position: usize,
        replacement: Pubkey,
        include_hub_authorizer: bool,
        expected_boundary: &'static str,
    }

    let mut cases = vec![
        (
            "Kamino withdraw source wrong mint",
            0,
            6,
            spoof("withdraw_source_wrong_mint"),
        ),
        (
            "Kamino withdraw source wrong owner",
            0,
            6,
            spoof("withdraw_source_wrong_owner"),
        ),
        (
            "Kamino withdraw source system account",
            0,
            6,
            system_account,
        ),
        (
            "Kamino withdraw source mint account",
            0,
            6,
            mint_as_token_account,
        ),
        (
            "Kamino withdraw source short token data",
            0,
            6,
            short_token_account,
        ),
        (
            "Kamino withdraw destination wrong mint",
            0,
            9,
            spoof("withdraw_destination_wrong_mint"),
        ),
        (
            "Kamino withdraw destination wrong owner",
            0,
            9,
            spoof("withdraw_destination_wrong_owner"),
        ),
        (
            "Kamino withdraw destination system account",
            0,
            9,
            system_account,
        ),
        (
            "Kamino withdraw destination mint account",
            0,
            9,
            mint_as_token_account,
        ),
        (
            "Kamino withdraw destination short token data",
            0,
            9,
            short_token_account,
        ),
        (
            "Kamino deposit source wrong mint",
            1,
            9,
            spoof("deposit_source_wrong_mint"),
        ),
        (
            "Kamino deposit source wrong owner",
            1,
            9,
            spoof("deposit_source_wrong_owner"),
        ),
        ("Kamino deposit source system account", 1, 9, system_account),
        (
            "Kamino deposit source mint account",
            1,
            9,
            mint_as_token_account,
        ),
        (
            "Kamino deposit source short token data",
            1,
            9,
            short_token_account,
        ),
        (
            "Kamino deposit destination wrong mint",
            1,
            8,
            spoof("deposit_destination_wrong_mint"),
        ),
        (
            "Kamino deposit destination wrong owner",
            1,
            8,
            spoof("deposit_destination_wrong_owner"),
        ),
        (
            "Kamino deposit destination system account",
            1,
            8,
            system_account,
        ),
        (
            "Kamino deposit destination mint account",
            1,
            8,
            mint_as_token_account,
        ),
        (
            "Kamino deposit destination short token data",
            1,
            8,
            short_token_account,
        ),
    ]
    .into_iter()
    .map(
        |(name, instruction_index, account_position, replacement)| SpoofCase {
            name,
            payload_builder: AdversarialRouteFixture::raw_same_mint_route,
            instruction_index,
            account_position,
            replacement,
            include_hub_authorizer: false,
            expected_boundary: "Kamino account-data boundary",
        },
    )
    .collect::<Vec<_>>();

    cases.extend(
        [
            (
                "Jupiter input wrong mint",
                1,
                1,
                spoof("jupiter_input_wrong_mint"),
            ),
            (
                "Jupiter input wrong owner",
                1,
                1,
                spoof("jupiter_input_wrong_owner"),
            ),
            ("Jupiter input system account", 1, 1, system_account),
            ("Jupiter input short token data", 1, 1, short_token_account),
            (
                "Jupiter output wrong mint",
                1,
                2,
                spoof("jupiter_output_wrong_mint"),
            ),
            (
                "Jupiter output wrong owner",
                1,
                2,
                spoof("jupiter_output_wrong_owner"),
            ),
            ("Jupiter output system account", 1, 2, system_account),
            ("Jupiter output mint account", 1, 2, mint_as_token_account),
        ]
        .into_iter()
        .map(
            |(name, instruction_index, account_position, replacement)| SpoofCase {
                name,
                payload_builder: AdversarialRouteFixture::raw_jupiter_route,
                instruction_index,
                account_position,
                replacement,
                include_hub_authorizer: false,
                expected_boundary: "Jupiter account-data boundary",
            },
        ),
    );
    cases.extend(
        [
            (
                "Hub user input wrong mint",
                loyal_actions::hub_abi::swap_exact_in_accounts::USER_INPUT as usize,
                spoof("hub_user_input_wrong_mint"),
            ),
            (
                "Hub user input wrong owner",
                loyal_actions::hub_abi::swap_exact_in_accounts::USER_INPUT as usize,
                spoof("hub_user_input_wrong_owner"),
            ),
            (
                "Hub user input system account",
                loyal_actions::hub_abi::swap_exact_in_accounts::USER_INPUT as usize,
                system_account,
            ),
            (
                "Hub user output wrong mint",
                loyal_actions::hub_abi::swap_exact_in_accounts::USER_OUTPUT as usize,
                spoof("hub_user_output_wrong_mint"),
            ),
            (
                "Hub user output wrong owner",
                loyal_actions::hub_abi::swap_exact_in_accounts::USER_OUTPUT as usize,
                spoof("hub_user_output_wrong_owner"),
            ),
            (
                "Hub user output short token data",
                loyal_actions::hub_abi::swap_exact_in_accounts::USER_OUTPUT as usize,
                short_token_account,
            ),
            (
                "Hub canonical input inventory wrong PDA",
                loyal_actions::hub_abi::swap_exact_in_accounts::HUB_INPUT as usize,
                spoof("hub_wrong_input_inventory"),
            ),
            (
                "Hub canonical output inventory wrong PDA",
                loyal_actions::hub_abi::swap_exact_in_accounts::HUB_OUTPUT as usize,
                spoof("hub_wrong_output_inventory"),
            ),
        ]
        .into_iter()
        .map(|(name, account_position, replacement)| SpoofCase {
            name,
            payload_builder: AdversarialRouteFixture::raw_hub_route,
            instruction_index: 1,
            account_position,
            replacement,
            include_hub_authorizer: true,
            expected_boundary: "Hub account-data or canonical-PDA boundary",
        }),
    );

    for case in cases {
        let mut payload = (case.payload_builder)(&fixture);
        replace_compiled_account(
            &mut payload,
            case.instruction_index,
            case.account_position,
            case.replacement,
        );
        let before = fixture.custody_snapshot_with_extra(&extra_accounts);
        let result = fixture.send_raw_route(payload, case.include_hub_authorizer);
        assert!(
            result.is_err(),
            "{} should reject at {}",
            case.name,
            case.expected_boundary
        );
        fixture.assert_custody_unchanged_with_extra(&before, &extra_accounts);
    }
}

#[test]
fn cross_vault_raw_payloads_do_not_satisfy_vault_zero_policy() {
    let Some(mut fixture) = setup_fixture() else {
        return;
    };

    let before = fixture.custody_snapshot();
    let mut payload = fixture.raw_same_mint_route();
    let second_vault_index = push_or_update_account_meta(
        &mut payload.transaction_accounts,
        AccountMeta::new(
            derive_squads_vault(&fixture.context.pool.settings, 1).0,
            false,
        ),
    );
    let second_source_index = push_or_update_account_meta(
        &mut payload.transaction_accounts,
        AccountMeta::new(fixture.second_vault_main_usdc_collateral, false),
    );
    let second_liquidity_index = push_or_update_account_meta(
        &mut payload.transaction_accounts,
        AccountMeta::new(fixture.second_vault_usdc, false),
    );
    let second_destination_index = push_or_update_account_meta(
        &mut payload.transaction_accounts,
        AccountMeta::new(fixture.second_vault_prime_usdc_collateral, false),
    );
    payload.compiled_instructions[0].accounts[0] = second_vault_index;
    payload.compiled_instructions[0].accounts[7] = second_source_index;
    payload.compiled_instructions[0].accounts[8] = second_liquidity_index;
    payload.compiled_instructions[1].accounts[0] = second_vault_index;
    payload.compiled_instructions[1].accounts[7] = second_liquidity_index;
    payload.compiled_instructions[1].accounts[8] = second_destination_index;
    let result = fixture.send_raw_route(payload, false);
    assert!(
        result.is_err(),
        "vault 0 policy should reject vault 1 token topology"
    );
    fixture.assert_custody_unchanged(&before);

    let before = fixture.custody_snapshot();
    let mut payload = fixture.raw_same_mint_route();
    let second_vault_liquidity_index = push_or_update_account_meta(
        &mut payload.transaction_accounts,
        AccountMeta::new(fixture.second_vault_usdc, false),
    );
    payload.compiled_instructions[1].accounts[7] = second_vault_liquidity_index;
    let result = fixture.send_raw_route(payload, false);
    assert!(
        result.is_err(),
        "same delegated signer cannot mix vault 1 SPL accounts into vault 0 route"
    );
    fixture.assert_custody_unchanged(&before);
}

fn hub_swap(
    fixture: &AdversarialRouteFixture,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> HubSwapExecution {
    HubSwapExecution {
        signer: fixture.wallet_b.pubkey(),
        vault_index: fixture.context.vault_index,
        vault: fixture.context.vault,
        vault_input,
        vault_output,
        input_mint,
        output_mint,
        hub_authorizer: fixture.hub_authorizer.pubkey(),
        amount_in: AMOUNT,
        amount_out: HUB_OUT,
        min_out: HUB_OUT,
        max_fee_bps: MAX_FEE_BPS,
        lane_id: 0,
    }
}

fn jupiter_transaction_accounts(
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    program_id: Pubkey,
) -> Vec<AccountMeta> {
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
        AccountMeta::new_readonly(
            squads_test_harness::derive_mock_jupiter_swap_authority(),
            false,
        ),
        AccountMeta::new_readonly(program_id, false),
    ]
}

fn raw_route_from_parts(
    parts: Vec<(Vec<SquadsCompiledInstruction>, Vec<AccountMeta>)>,
    instruction_constraint_indices: Vec<u8>,
) -> RawRoutePayload {
    let mut transaction_accounts = Vec::new();
    let mut compiled_instructions = Vec::new();
    for (instructions, accounts) in parts {
        compiled_instructions.extend(merge_compiled_instructions_for_test(
            &mut transaction_accounts,
            instructions,
            accounts,
        ));
    }
    RawRoutePayload {
        compiled_instructions,
        instruction_constraint_indices,
        transaction_accounts,
    }
}

fn raw_jupiter_swap_part(
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    program_id: Pubkey,
) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
    (
        vec![SquadsCompiledInstruction {
            program_id_index: 9,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8],
            data: mock_jupiter_stable_exact_in_swap_data(AMOUNT, AMOUNT, input_mint, output_mint),
        }],
        jupiter_transaction_accounts(
            vault,
            vault_input,
            vault_output,
            input_mint,
            output_mint,
            program_id,
        ),
    )
}

fn raw_hub_swap_part(args: HubSwapExecution) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
    let mut instruction = loyal_actions::loyal_hub_swap_exact_in_instruction(
        args.vault,
        args.vault_input,
        args.vault_output,
        args.input_mint,
        args.output_mint,
        args.hub_authorizer,
        loyal_actions::LoyalHubSwapExactIn {
            amount_in: args.amount_in,
            amount_out: args.amount_out,
            min_out: args.min_out,
            max_fee_bps: args.max_fee_bps,
            lane_id: args.lane_id,
        },
    );
    instruction.accounts[1].is_signer = false;
    let mut accounts = instruction.accounts;
    accounts.push(AccountMeta::new_readonly(instruction.program_id, false));
    (
        vec![SquadsCompiledInstruction {
            program_id_index: accounts.len() - 1,
            accounts: (0..accounts.len() - 1).collect(),
            data: instruction.data,
        }],
        accounts,
    )
}

fn merge_compiled_instructions_for_test(
    transaction_accounts: &mut Vec<AccountMeta>,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    source_accounts: Vec<AccountMeta>,
) -> Vec<SquadsCompiledInstruction> {
    compiled_instructions
        .into_iter()
        .map(|instruction| {
            let program_id_index = remap_account_index_for_test(
                transaction_accounts,
                &source_accounts,
                instruction.program_id_index,
            );
            let accounts = instruction
                .accounts
                .into_iter()
                .map(|index| {
                    remap_account_index_for_test(transaction_accounts, &source_accounts, index)
                })
                .collect();
            SquadsCompiledInstruction {
                program_id_index,
                accounts,
                data: instruction.data,
            }
        })
        .collect()
}

fn remap_account_index_for_test(
    transaction_accounts: &mut Vec<AccountMeta>,
    source_accounts: &[AccountMeta],
    index: usize,
) -> usize {
    push_or_update_account_meta(transaction_accounts, source_accounts[index].clone())
}

fn push_or_update_account_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> usize {
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

fn replace_compiled_account(
    payload: &mut RawRoutePayload,
    instruction_index: usize,
    account_position: usize,
    pubkey: Pubkey,
) {
    let old_index = payload.compiled_instructions[instruction_index].accounts[account_position];
    let mut meta = payload.transaction_accounts[old_index].clone();
    meta.pubkey = pubkey;
    let new_index = push_or_update_account_meta(&mut payload.transaction_accounts, meta);
    payload.compiled_instructions[instruction_index].accounts[account_position] = new_index;
}

fn flip_compiled_account_signer(
    payload: &mut RawRoutePayload,
    instruction_index: usize,
    account_position: usize,
) {
    let account_index = payload.compiled_instructions[instruction_index].accounts[account_position];
    payload.transaction_accounts[account_index].is_signer =
        !payload.transaction_accounts[account_index].is_signer;
}

fn flip_compiled_account_writable(
    payload: &mut RawRoutePayload,
    instruction_index: usize,
    account_position: usize,
) {
    let account_index = payload.compiled_instructions[instruction_index].accounts[account_position];
    payload.transaction_accounts[account_index].is_writable =
        !payload.transaction_accounts[account_index].is_writable;
}

fn flip_compiled_program_signer(payload: &mut RawRoutePayload, instruction_index: usize) {
    let account_index = payload.compiled_instructions[instruction_index].program_id_index;
    payload.transaction_accounts[account_index].is_signer =
        !payload.transaction_accounts[account_index].is_signer;
}

fn copy_compiled_instruction(instruction: &SquadsCompiledInstruction) -> SquadsCompiledInstruction {
    SquadsCompiledInstruction {
        program_id_index: instruction.program_id_index,
        accounts: instruction.accounts.clone(),
        data: instruction.data.clone(),
    }
}
