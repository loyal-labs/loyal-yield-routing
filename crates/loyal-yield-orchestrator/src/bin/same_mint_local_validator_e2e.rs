use std::{
    fs::{self, File, OpenOptions},
    io::Write,
    net::TcpListener,
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    thread::sleep,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use anyhow::{anyhow, bail, Context, Result};
use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use chrono::Utc;
use loyal_actions::{
    create_all_in_one_market_mint_yield_route_action, JupiterSwapContract, LoyalActionContext,
    SwapLane, YieldRouteUniverse, JUPITER_DEFAULT_MAX_SLIPPAGE_BPS, JUPITER_V6_PROGRAM_ID,
    YIELD_ROUTE_WITHDRAW_ACTION_SEED,
};
use loyal_yield_orchestrator::{
    latest_same_mint_apys, run_same_mint_yield_routing_loop, ConfiguredSameMintRoute,
    ConfiguredSameMintRoutePreparer, KaminoReserveInstructionAccounts, MainnetSameMintExecutor,
    MainnetSameMintExecutorConfig, NeonSqlConfig, OrchestratorStore, PlannerConfig,
    PolicyMatchInput, ReconciledReservePosition, ReconciledVaultState, SameMintRouteQuoteConfig,
    SameMintRoutingLoopConfig,
};
use loyal_yield_router::timescale::{
    ReserveUpdateFilter, SubscribeOptions, TimescaleRouterClient, TimescaleRouterClientConfig,
};
use serde::Serialize;
use serde_json::json;
use solana_rpc_client::rpc_client::RpcClient;
use solana_sdk::{
    account::Account,
    compute_budget::ComputeBudgetInstruction,
    hash::hashv,
    instruction::Instruction,
    pubkey::Pubkey,
    signature::{read_keypair_file, Keypair, Signature, Signer},
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use spl_token::solana_program::{program_option::COption, program_pack::Pack};
use sqlx::Row;
use squads_test_harness::{
    create_squads_smart_account_instruction, derive_mock_kamino_lending_market_authority,
    derive_squads_pool, derive_squads_vault, execute_squads_sync_transaction_instruction,
    mock_kamino_collateral_mint, mock_kamino_deposit_reserve_liquidity_data,
    mock_kamino_reserve_transaction, serialize_squads_program_config, squads_test_treasury,
    MockKaminoReserveTokenAccounts, SquadsPool, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE,
    KAMINO_PRIME_MARKET, KAMINO_PRIME_USDC_RESERVE, KAMINO_RESERVE_STATE_LEN, LAMPORTS_PER_SOL,
    MOCK_JUPITER_STABLE_EXACT_IN, MOCK_YIELD_PROTOCOLS_PROGRAM_SO,
    SQUADS_EXTENDED_HEAP_FRAME_BYTES, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    SQUADS_SMART_ACCOUNT_PROGRAM_SO_FIXTURE, USDC_DECIMALS, USDC_MINT,
};
use tokio::time::timeout;

const AMOUNT: u64 = 1_000_000;
const SMART_ACCOUNT_SEED: u128 = 42;
const VAULT_INDEX: u8 = 0;
const WATCH_TIMEOUT: Duration = Duration::from_secs(30);
const LOCAL_VALIDATOR_RPC_PORT_ENV: &str = "SAME_MINT_LOCAL_VALIDATOR_RPC_PORT";
const LOCAL_VALIDATOR_FAUCET_PORT_ENV: &str = "SAME_MINT_LOCAL_VALIDATOR_FAUCET_PORT";
const LOCAL_VALIDATOR_BIN_ENV: &str = "SAME_MINT_LOCAL_VALIDATOR_BIN";
const DEFAULT_LOCAL_VALIDATOR_RPC_PORT: u16 = 8895;
const DEFAULT_LOCAL_VALIDATOR_FAUCET_PORT: u16 = 9895;

#[tokio::main]
async fn main() {
    if let Err(error) = run().await {
        eprintln!("{error:?}");
        std::process::exit(1);
    }
}

async fn run() -> Result<()> {
    let work_dir = create_work_dir()?;
    println!("local same-mint E2E work dir: {}", work_dir.display());

    let postgres = TempPostgres::start(&work_dir).context("start temp TimescaleDB")?;
    let database_url = postgres.database_url.clone();
    let store = OrchestratorStore::connect(NeonSqlConfig::new(database_url.clone())).await?;
    store.apply_migrations().await?;
    create_local_timescale_schema(store.pool()).await?;

    let wallet = local_cli_keypair().unwrap_or_else(|_| Keypair::new());
    let router = Keypair::new();
    let pool = derive_squads_pool(SMART_ACCOUNT_SEED);
    let (vault, _) = derive_squads_vault(&pool.settings, VAULT_INDEX);
    let accounts = LocalAccounts::new();
    write_validator_accounts(&work_dir, &wallet, pool, &accounts)?;

    let mut validator = LocalValidator::start(&work_dir, None)?;
    let rpc = RpcClient::new_with_timeout(validator.rpc_url.clone(), Duration::from_secs(30));
    wait_for_validator(&rpc, &mut validator.child_handle())?;
    print_program_account(&rpc, SQUADS_SMART_ACCOUNT_PROGRAM_ID, "Squads")?;
    print_program_account(&rpc, loyal_actions::KAMINO_LEND_PROGRAM_ID, "mock Kamino")?;

    send_setup_transactions(&rpc, &wallet, &router, pool, vault, &accounts)
        .context("seed local validator policy and starting Kamino position")?;

    let setup_signature = seed_orchestrator_state(&store, &wallet, &router, pool, vault, &accounts)
        .await
        .context("seed orchestrator DB state")?;
    println!("seeded orchestrator policy from setup signature {setup_signature}");

    let filter = ReserveUpdateFilter::new()
        .with_symbols(["USDC"])
        .with_changed_fields(["supply_apy"])
        .with_stale(false);
    let timescale =
        TimescaleRouterClient::connect(TimescaleRouterClientConfig::new(database_url)).await?;
    insert_reserve_update(store.pool(), &accounts.main, 1, 0.0100, false).await?;
    insert_reserve_update(store.pool(), &accounts.prime, 2, 0.0050, false).await?;
    let mut stream = timescale
        .subscribe(filter.clone(), SubscribeOptions::default())
        .await?;

    insert_reserve_update(store.pool(), &accounts.prime, 3, 0.0800, true).await?;
    let item = timeout(WATCH_TIMEOUT, stream.next_update())
        .await
        .map_err(|_| anyhow!("timed out waiting for local Timescale APY notification"))??;
    println!(
        "received APY notification reserve={} slot={} supply_apy={}",
        item.row.reserve, item.row.slot, item.row.supply_apy
    );

    let reserve_apys = latest_same_mint_apys(&timescale, filter).await?;
    let preparer = route_preparer(store.pool(), router.pubkey(), vault, VAULT_INDEX, &accounts)
        .await
        .context("build local route preparer")?;
    let executor_config = MainnetSameMintExecutorConfig::new(validator.rpc_url.clone())
        .with_submit_transactions(true);
    let executor = MainnetSameMintExecutor::new(executor_config, router, preparer);
    let report = run_same_mint_yield_routing_loop(
        &store,
        &executor,
        reserve_apys,
        SameMintRoutingLoopConfig {
            planner: PlannerConfig {
                min_edge_bps: 1,
                estimated_cost_lamports: 0,
            },
            batch_size: 1,
            submit_batches: true,
        },
    )
    .await?;

    println!("{}", serde_json::to_string_pretty(&report)?);
    if report.submitted_decisions.is_empty() || !report.failed_decisions.is_empty() {
        bail!("same-mint local route did not submit successfully");
    }

    let balances = TokenBalances::fetch(&rpc, &accounts)?;
    balances.assert_routed()?;
    assert_db_submission(store.pool()).await?;

    println!("same-mint local validator E2E passed");
    drop(validator);
    drop(postgres);
    Ok(())
}

fn send_setup_transactions(
    rpc: &RpcClient,
    wallet: &Keypair,
    router: &Keypair,
    pool: SquadsPool,
    vault: Pubkey,
    accounts: &LocalAccounts,
) -> Result<()> {
    let create_smart_account_ix =
        create_squads_smart_account_instruction(wallet.pubkey(), wallet.pubkey(), pool.seed);
    send_instructions(rpc, &[create_smart_account_ix], wallet, false)
        .context("create Squads smart account")?;

    let fund_vault_ix = system_instruction::transfer(&wallet.pubkey(), &vault, LAMPORTS_PER_SOL);
    let fund_router_ix =
        system_instruction::transfer(&wallet.pubkey(), &router.pubkey(), LAMPORTS_PER_SOL / 10);
    send_instructions(rpc, &[fund_vault_ix, fund_router_ix], wallet, false)
        .context("fund vault and router signer")?;

    let route_setup = create_route_action_setup(wallet.pubkey(), router.pubkey(), pool, vault)?;
    send_instructions(rpc, &route_setup.instructions, wallet, true)
        .context("create route policy")?;

    let (deposit_instructions, deposit_accounts) = mock_kamino_reserve_transaction(
        vault,
        accounts.main.to_mock(),
        mock_kamino_deposit_reserve_liquidity_data(AMOUNT),
    );
    let deposit_ix = execute_squads_sync_transaction_instruction(
        pool.settings,
        wallet.pubkey(),
        VAULT_INDEX,
        deposit_instructions,
        deposit_accounts,
    );
    send_instructions(rpc, &[deposit_ix], wallet, true).context("seed starting Kamino deposit")?;
    Ok(())
}

async fn seed_orchestrator_state(
    store: &OrchestratorStore,
    wallet: &Keypair,
    router: &Keypair,
    pool: SquadsPool,
    vault: Pubkey,
    accounts: &LocalAccounts,
) -> Result<String> {
    let route_setup = create_route_action_setup(wallet.pubkey(), router.pubkey(), pool, vault)?;
    let policy_account = route_setup.same_mint_route()?.action_account();
    let signature = Signature::new_unique().to_string();
    let stored = store
        .record_policy_match(PolicyMatchInput {
            signature: signature.clone(),
            slot: 1,
            cluster: "localnet".to_owned(),
            settings: pool.settings.to_string(),
            authority: wallet.pubkey().to_string(),
            policy_seed: YIELD_ROUTE_WITHDRAW_ACTION_SEED,
            policy_account: policy_account.to_string(),
            vault_index: VAULT_INDEX,
            vault_pubkey: vault.to_string(),
            delegated_signers: vec![router.pubkey().to_string()],
            threshold: 1,
            route_modes: vec!["same_mint".to_owned()],
            stable_mints: vec![USDC_MINT.to_string()],
            kamino_markets: vec![
                KAMINO_MAIN_MARKET.to_string(),
                KAMINO_PRIME_MARKET.to_string(),
            ],
            kamino_liquidity_mints: vec![USDC_MINT.to_string()],
            universe_preset: Some("local-usdc".to_owned()),
            risk_profile: Some("local-e2e".to_owned()),
            swap_lanes: json!([{ "type": "mock-jupiter" }]),
        })
        .await?;

    store
        .reconcile_vault(
            stored.vault.id,
            ReconciledVaultState {
                observed_slot: 2,
                observed_at: Some(Utc::now()),
                chain_slot: None,
                lock_attempt_id: None,
                context: json!({ "source": "same_mint_local_validator_e2e" }),
                positions: vec![
                    ReconciledReservePosition {
                        reserve: accounts.main.reserve.to_string(),
                        market: Some(accounts.main.market.to_string()),
                        liquidity_mint: USDC_MINT.to_string(),
                        amount_raw: AMOUNT,
                        supply_apy_bps: Some(100),
                        borrow_apy_bps: Some(0),
                        planning_metadata: json!({ "collateral_account": accounts.main.vault_collateral.to_string() }),
                    },
                    ReconciledReservePosition {
                        reserve: accounts.prime.reserve.to_string(),
                        market: Some(accounts.prime.market.to_string()),
                        liquidity_mint: USDC_MINT.to_string(),
                        amount_raw: 0,
                        supply_apy_bps: Some(50),
                        borrow_apy_bps: Some(0),
                        planning_metadata: json!({ "collateral_account": accounts.prime.vault_collateral.to_string() }),
                    },
                ],
            },
        )
        .await?;

    Ok(signature)
}

async fn route_preparer(
    pool: &sqlx::PgPool,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
    accounts: &LocalAccounts,
) -> Result<ConfiguredSameMintRoutePreparer> {
    let row = sqlx::query(
        r#"
        SELECT vault.id AS vault_id, policy.policy_account
        FROM loyal_yield.managed_vaults vault
        JOIN loyal_yield.route_policies policy ON policy.id = vault.active_policy_id
        WHERE vault.active
        ORDER BY vault.id DESC
        LIMIT 1
        "#,
    )
    .fetch_one(pool)
    .await?;
    let vault_id: i64 = row.try_get("vault_id")?;
    let policy_account: String = row.try_get("policy_account")?;

    let route_setup = create_route_action_setup(
        Pubkey::new_unique(),
        delegated_signer,
        derive_squads_pool(SMART_ACCOUNT_SEED),
        vault,
    )?;
    let same_mint = route_setup.same_mint_route()?;
    let indexes = same_mint.instruction_constraint_indexes();
    ConfiguredSameMintRoutePreparer::new(vec![ConfiguredSameMintRoute {
        vault_id: Some(loyal_yield_orchestrator::VaultId(vault_id)),
        source_reserve: accounts.main.reserve.to_string(),
        target_reserve: accounts.prime.reserve.to_string(),
        liquidity_mint: USDC_MINT.to_string(),
        policy_account: policy_account.parse()?,
        delegated_signer,
        vault_index,
        vault,
        withdraw_constraint_index: indexes[0],
        deposit_constraint_index: indexes[1],
        source_accounts: accounts.main.to_instruction_accounts(),
        target_accounts: accounts.prime.to_instruction_accounts(),
        quote: SameMintRouteQuoteConfig {
            redeem_collateral_to_liquidity_bps: 10_000,
            deposit_liquidity_bps: 10_000,
            max_redeem_collateral_raw: Some(AMOUNT),
            min_deposit_liquidity_raw: Some(AMOUNT),
        },
    }])
    .map_err(Into::into)
}

fn create_route_action_setup(
    authority: Pubkey,
    delegated_signer: Pubkey,
    pool: SquadsPool,
    vault: Pubkey,
) -> Result<loyal_actions::YieldRouteActionSetup> {
    create_all_in_one_market_mint_yield_route_action(
        LoyalActionContext {
            settings: pool.settings,
            authority,
            delegated_signer,
            account_index: VAULT_INDEX,
            vault,
        },
        YieldRouteUniverse::new(
            vec![USDC_MINT],
            vec![KAMINO_MAIN_MARKET, KAMINO_PRIME_MARKET],
            vec![USDC_MINT],
        ),
        vec![SwapLane::Jupiter(JupiterSwapContract {
            program_id: JUPITER_V6_PROGRAM_ID,
            exact_in_discriminator: MOCK_JUPITER_STABLE_EXACT_IN,
            max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
        })],
    )
    .map_err(Into::into)
}

fn send_instructions(
    rpc: &RpcClient,
    instructions: &[Instruction],
    payer: &Keypair,
    heap_frame: bool,
) -> Result<Signature> {
    let mut all = Vec::with_capacity(instructions.len() + usize::from(heap_frame));
    if heap_frame {
        all.push(ComputeBudgetInstruction::request_heap_frame(
            SQUADS_EXTENDED_HEAP_FRAME_BYTES,
        ));
    }
    all.extend_from_slice(instructions);

    let blockhash = rpc.get_latest_blockhash()?;
    let transaction =
        Transaction::new_signed_with_payer(&all, Some(&payer.pubkey()), &[payer], blockhash);
    rpc.send_and_confirm_transaction(&transaction)
        .context("send and confirm local validator transaction")
}

#[derive(Clone, Copy)]
struct LocalReserve {
    reserve: Pubkey,
    market: Pubkey,
    lending_market_authority: Pubkey,
    liquidity_mint: Pubkey,
    collateral_mint: Pubkey,
    reserve_liquidity_supply: Pubkey,
    vault_liquidity: Pubkey,
    vault_collateral: Pubkey,
}

impl LocalReserve {
    fn to_instruction_accounts(self) -> KaminoReserveInstructionAccounts {
        KaminoReserveInstructionAccounts {
            reserve: self.reserve,
            market: self.market,
            lending_market_authority: self.lending_market_authority,
            liquidity_mint: self.liquidity_mint,
            reserve_liquidity_supply: self.reserve_liquidity_supply,
            collateral_mint: self.collateral_mint,
            vault_liquidity: self.vault_liquidity,
            vault_collateral: self.vault_collateral,
        }
    }

    fn to_mock(self) -> MockKaminoReserveTokenAccounts {
        MockKaminoReserveTokenAccounts {
            reserve: self.reserve,
            market: self.market,
            lending_market_authority: self.lending_market_authority,
            liquidity_mint: self.liquidity_mint,
            collateral_mint: self.collateral_mint,
            reserve_liquidity_authority: self.lending_market_authority,
            collateral_mint_authority: self.lending_market_authority,
            vault_liquidity: self.vault_liquidity,
            vault_collateral: self.vault_collateral,
            reserve_liquidity_supply: self.reserve_liquidity_supply,
        }
    }
}

struct LocalAccounts {
    main: LocalReserve,
    prime: LocalReserve,
}

impl LocalAccounts {
    fn new() -> Self {
        let vault_liquidity = deterministic_pubkey(b"local-e2e-vault-usdc");
        let main = reserve_accounts(
            KAMINO_MAIN_USDC_RESERVE,
            KAMINO_MAIN_MARKET,
            vault_liquidity,
            deterministic_pubkey(b"local-e2e-main-collateral"),
            deterministic_pubkey(b"local-e2e-main-reserve-supply"),
        );
        let prime = reserve_accounts(
            KAMINO_PRIME_USDC_RESERVE,
            KAMINO_PRIME_MARKET,
            vault_liquidity,
            deterministic_pubkey(b"local-e2e-prime-collateral"),
            deterministic_pubkey(b"local-e2e-prime-reserve-supply"),
        );
        Self { main, prime }
    }
}

fn reserve_accounts(
    reserve: Pubkey,
    market: Pubkey,
    vault_liquidity: Pubkey,
    vault_collateral: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> LocalReserve {
    LocalReserve {
        reserve,
        market,
        lending_market_authority: derive_mock_kamino_lending_market_authority(market),
        liquidity_mint: USDC_MINT,
        collateral_mint: mock_kamino_collateral_mint(reserve),
        reserve_liquidity_supply,
        vault_liquidity,
        vault_collateral,
    }
}

fn deterministic_pubkey(seed: &[u8]) -> Pubkey {
    Pubkey::new_from_array(hashv(&[seed]).to_bytes())
}

fn write_validator_accounts(
    work_dir: &Path,
    wallet: &Keypair,
    pool: SquadsPool,
    accounts: &LocalAccounts,
) -> Result<()> {
    let accounts_dir = work_dir.join("accounts");
    fs::create_dir_all(&accounts_dir)?;
    write_account_json(
        &accounts_dir,
        pool.settings,
        SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        serialize_squads_program_config(wallet.pubkey(), squads_test_treasury(), 0),
        LAMPORTS_PER_SOL,
        false,
    )?;
    write_empty_system_account(&accounts_dir, squads_test_treasury())?;

    let vault = derive_squads_vault(&pool.settings, VAULT_INDEX).0;
    write_spl_mint(&accounts_dir, USDC_MINT, None, USDC_DECIMALS, AMOUNT)?;
    write_spl_token_account(
        &accounts_dir,
        accounts.main.vault_liquidity,
        USDC_MINT,
        vault,
        AMOUNT,
    )?;
    for reserve in [accounts.main, accounts.prime] {
        write_empty_system_account(&accounts_dir, reserve.market)?;
        write_empty_system_account(&accounts_dir, reserve.lending_market_authority)?;
        write_kamino_reserve_state(&accounts_dir, reserve)?;
        write_spl_mint(
            &accounts_dir,
            reserve.collateral_mint,
            Some(reserve.lending_market_authority),
            USDC_DECIMALS,
            0,
        )?;
        write_spl_token_account(
            &accounts_dir,
            reserve.vault_collateral,
            reserve.collateral_mint,
            vault,
            0,
        )?;
        write_spl_token_account(
            &accounts_dir,
            reserve.reserve_liquidity_supply,
            USDC_MINT,
            reserve.lending_market_authority,
            0,
        )?;
    }
    Ok(())
}

fn write_kamino_reserve_state(accounts_dir: &Path, reserve: LocalReserve) -> Result<()> {
    let mut data = vec![0; KAMINO_RESERVE_STATE_LEN];
    data[0..32].copy_from_slice(reserve.market.as_ref());
    data[32..64].copy_from_slice(reserve.liquidity_mint.as_ref());
    data[64..96].copy_from_slice(reserve.collateral_mint.as_ref());
    data[96..128].copy_from_slice(reserve.reserve_liquidity_supply.as_ref());
    write_account_json(
        accounts_dir,
        reserve.reserve,
        solana_sdk::system_program::ID,
        data,
        LAMPORTS_PER_SOL,
        false,
    )
}

fn write_empty_system_account(accounts_dir: &Path, pubkey: Pubkey) -> Result<()> {
    write_account_json(
        accounts_dir,
        pubkey,
        solana_sdk::system_program::ID,
        Vec::new(),
        LAMPORTS_PER_SOL,
        false,
    )
}

fn write_spl_mint(
    accounts_dir: &Path,
    pubkey: Pubkey,
    mint_authority: Option<Pubkey>,
    decimals: u8,
    supply: u64,
) -> Result<()> {
    let mut data = vec![0; spl_token::state::Mint::LEN];
    spl_token::state::Mint {
        mint_authority: mint_authority.map_or(COption::None, COption::Some),
        supply,
        decimals,
        is_initialized: true,
        freeze_authority: COption::None,
    }
    .pack_into_slice(&mut data);
    write_account_json(
        accounts_dir,
        pubkey,
        spl_token::id(),
        data,
        LAMPORTS_PER_SOL,
        false,
    )
}

fn write_spl_token_account(
    accounts_dir: &Path,
    pubkey: Pubkey,
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
) -> Result<()> {
    let mut data = vec![0; spl_token::state::Account::LEN];
    spl_token::state::Account {
        mint,
        owner,
        amount,
        delegate: COption::None,
        state: spl_token::state::AccountState::Initialized,
        is_native: COption::None,
        delegated_amount: 0,
        close_authority: COption::None,
    }
    .pack_into_slice(&mut data);
    write_account_json(
        accounts_dir,
        pubkey,
        spl_token::id(),
        data,
        LAMPORTS_PER_SOL,
        false,
    )
}

fn write_account_json(
    accounts_dir: &Path,
    pubkey: Pubkey,
    owner: Pubkey,
    data: Vec<u8>,
    lamports: u64,
    executable: bool,
) -> Result<()> {
    let dump = AccountDump {
        pubkey: pubkey.to_string(),
        account: AccountDumpAccount {
            lamports,
            data: [BASE64.encode(&data), "base64".to_owned()],
            owner: owner.to_string(),
            executable,
            rent_epoch: 0,
            space: data.len(),
        },
    };
    let path = accounts_dir.join(format!("{pubkey}.json"));
    fs::write(path, serde_json::to_vec_pretty(&dump)?)?;
    Ok(())
}

#[derive(Serialize)]
struct AccountDump {
    pubkey: String,
    account: AccountDumpAccount,
}

#[derive(Serialize)]
struct AccountDumpAccount {
    lamports: u64,
    data: [String; 2],
    owner: String,
    executable: bool,
    #[serde(rename = "rentEpoch")]
    rent_epoch: u64,
    space: usize,
}

struct LocalValidator {
    rpc_url: String,
    child: Child,
}

impl LocalValidator {
    fn start(work_dir: &Path, mint: Option<Pubkey>) -> Result<Self> {
        let (rpc_port, faucet_port) = validator_ports()?;
        let ledger = work_dir.join("validator-ledger");
        let log = File::create(work_dir.join("validator.log"))?;
        let squads_so = fs::canonicalize(SQUADS_SMART_ACCOUNT_PROGRAM_SO_FIXTURE)
            .context("find Squads SBF fixture")?;
        let mock_so = fs::canonicalize(find_mock_yield_program_so()?)
            .context("canonicalize mock yield SBF path")?;
        let account_dir = fs::canonicalize(work_dir.join("accounts"))?;
        let validator_bin = std::env::var(LOCAL_VALIDATOR_BIN_ENV)
            .unwrap_or_else(|_| "solana-test-validator".to_owned());
        let mut command = Command::new(validator_bin);
        command
            .arg("--reset")
            .arg("--quiet")
            .arg("--ledger")
            .arg(&ledger)
            .arg("--rpc-port")
            .arg(rpc_port.to_string())
            .arg("--faucet-port")
            .arg(faucet_port.to_string());
        if let Some(mint) = mint {
            command.arg("--mint").arg(mint.to_string());
        }
        let child = command
            .arg("--account-dir")
            .arg(account_dir)
            .arg("--upgradeable-program")
            .arg(SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string())
            .arg(squads_so)
            .arg("none")
            .arg("--upgradeable-program")
            .arg(loyal_actions::KAMINO_LEND_PROGRAM_ID.to_string())
            .arg(mock_so)
            .arg("none")
            .stdout(Stdio::null())
            .stderr(Stdio::from(log))
            .spawn()
            .context("spawn solana-test-validator")?;
        Ok(Self {
            rpc_url: format!("http://127.0.0.1:{rpc_port}"),
            child,
        })
    }

    fn child_handle(&mut self) -> &mut Child {
        &mut self.child
    }
}

impl Drop for LocalValidator {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn find_mock_yield_program_so() -> Result<PathBuf> {
    for path in [
        PathBuf::from("target/deploy").join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
        PathBuf::from("target/sbpf-solana-solana/release").join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
        PathBuf::from("target/sbpf-solana-solana/release/deps")
            .join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
    ] {
        if path.exists() {
            return Ok(path);
        }
    }
    bail!(
        "missing {}; run `cargo build-sbf -- -p mock-yield-protocols-program` first",
        MOCK_YIELD_PROTOCOLS_PROGRAM_SO
    )
}

fn wait_for_validator(rpc: &RpcClient, child: &mut Child) -> Result<()> {
    for _ in 0..60 {
        if let Some(status) = child.try_wait()? {
            bail!("solana-test-validator exited early with {status}");
        }
        if rpc.get_latest_blockhash().is_ok()
            && rpc.get_account(&SQUADS_SMART_ACCOUNT_PROGRAM_ID).is_ok()
            && rpc
                .get_account(&loyal_actions::KAMINO_LEND_PROGRAM_ID)
                .is_ok()
        {
            return Ok(());
        }
        sleep(Duration::from_secs(1));
    }
    bail!("timed out waiting for solana-test-validator RPC")
}

fn print_program_account(rpc: &RpcClient, program_id: Pubkey, label: &str) -> Result<()> {
    let account = rpc.get_account(&program_id)?;
    println!(
        "{label} program account owner={} executable={} data_len={}",
        account.owner,
        account.executable,
        account.data.len()
    );
    if account.data.len() == 36 {
        let mut programdata = [0; 32];
        programdata.copy_from_slice(&account.data[4..36]);
        let programdata = Pubkey::new_from_array(programdata);
        match rpc.get_account(&programdata) {
            Ok(programdata_account) => println!(
                "{label} programdata {} owner={} executable={} data_len={}",
                programdata,
                programdata_account.owner,
                programdata_account.executable,
                programdata_account.data.len()
            ),
            Err(error) => println!("{label} programdata {programdata} missing: {error}"),
        }
    }
    Ok(())
}

struct TempPostgres {
    database_url: String,
    data_dir: PathBuf,
}

impl TempPostgres {
    fn start(work_dir: &Path) -> Result<Self> {
        let pg_dir = work_dir.join("pgdata");
        let log = work_dir.join("postgres.log");
        let port = free_port()?;
        run_command(
            Command::new("initdb")
                .arg("-D")
                .arg(&pg_dir)
                .arg("-A")
                .arg("trust")
                .arg("--no-instructions"),
        )
        .context("initdb")?;

        let mut conf = OpenOptions::new()
            .append(true)
            .open(pg_dir.join("postgresql.conf"))?;
        writeln!(conf, "shared_preload_libraries = 'timescaledb'")?;
        writeln!(conf, "listen_addresses = '127.0.0.1'")?;

        run_command(
            Command::new("pg_ctl")
                .arg("-D")
                .arg(&pg_dir)
                .arg("-l")
                .arg(&log)
                .arg("-o")
                .arg(format!("-p {port} -h 127.0.0.1"))
                .arg("-w")
                .arg("start"),
        )
        .context("pg_ctl start")?;

        let database = "loyal_yield_e2e";
        run_command(
            Command::new("createdb")
                .arg("-h")
                .arg("127.0.0.1")
                .arg("-p")
                .arg(port.to_string())
                .arg(database),
        )
        .context("createdb")?;
        let user = std::env::var("USER").unwrap_or_else(|_| "postgres".to_owned());
        Ok(Self {
            database_url: format!("postgres://{user}@127.0.0.1:{port}/{database}"),
            data_dir: pg_dir,
        })
    }
}

impl Drop for TempPostgres {
    fn drop(&mut self) {
        let _ = Command::new("pg_ctl")
            .arg("-D")
            .arg(&self.data_dir)
            .arg("-m")
            .arg("fast")
            .arg("-w")
            .arg("stop")
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status();
    }
}

fn run_command(command: &mut Command) -> Result<()> {
    let output = command.output()?;
    if !output.status.success() {
        bail!(
            "command failed with status {}: {}\n{}",
            output.status,
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
    }
    Ok(())
}

async fn create_local_timescale_schema(pool: &sqlx::PgPool) -> Result<()> {
    sqlx::raw_sql(
        r#"
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE SCHEMA IF NOT EXISTS kamino;
CREATE TABLE IF NOT EXISTS kamino.reserve_updates (
    observed_at TIMESTAMPTZ NOT NULL,
    slot BIGINT NOT NULL,
    kind TEXT NOT NULL,
    source TEXT NOT NULL,
    reserve TEXT NOT NULL,
    market TEXT,
    market_name TEXT,
    symbol TEXT,
    liquidity_mint TEXT NOT NULL,
    mint_decimals INTEGER NOT NULL,
    reserve_last_update_slot BIGINT NOT NULL,
    reserve_last_update_stale BOOLEAN NOT NULL,
    reserve_price_status SMALLINT NOT NULL,
    available_amount DOUBLE PRECISION NOT NULL,
    borrowed_amount DOUBLE PRECISION NOT NULL,
    borrowed_amount_sf TEXT NOT NULL,
    total_supply_amount DOUBLE PRECISION NOT NULL,
    market_price_usd DOUBLE PRECISION NOT NULL,
    market_price_last_updated_ts BIGINT NOT NULL,
    cumulative_borrow_rate_bsf TEXT NOT NULL,
    total_supply_usd_estimate DOUBLE PRECISION NOT NULL,
    total_borrow_usd_estimate DOUBLE PRECISION NOT NULL,
    utilization DOUBLE PRECISION NOT NULL,
    borrow_apr DOUBLE PRECISION NOT NULL,
    supply_apr DOUBLE PRECISION NOT NULL,
    borrow_apy DOUBLE PRECISION NOT NULL,
    supply_apy DOUBLE PRECISION NOT NULL,
    protocol_take_rate_pct SMALLINT NOT NULL,
    host_fixed_interest_rate_bps INTEGER NOT NULL,
    diff_changed BOOLEAN NOT NULL,
    changed_fields TEXT[] NOT NULL DEFAULT '{}',
    diff_summary TEXT NOT NULL,
    diff JSONB NOT NULL,
    target JSONB NOT NULL,
    snapshot JSONB NOT NULL,
    record JSONB NOT NULL,
    raw_account_data_base64 TEXT,
    api_supply_apy DOUBLE PRECISION,
    api_borrow_apy DOUBLE PRECISION,
    api_total_supply_usd DOUBLE PRECISION,
    api_total_borrow_usd DOUBLE PRECISION
);
SELECT create_hypertable('kamino.reserve_updates', 'observed_at', if_not_exists => TRUE, chunk_time_interval => INTERVAL '1 day');
CREATE INDEX IF NOT EXISTS reserve_updates_reserve_time_idx ON kamino.reserve_updates (reserve, observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_symbol_time_idx ON kamino.reserve_updates (symbol, observed_at DESC);
CREATE INDEX IF NOT EXISTS reserve_updates_supply_apy_time_idx ON kamino.reserve_updates (supply_apy DESC, observed_at DESC);
CREATE OR REPLACE FUNCTION kamino.notify_reserve_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_notify(
        'kamino_reserve_updates',
        json_build_object(
            'observed_at', NEW.observed_at,
            'slot', NEW.slot,
            'reserve', NEW.reserve,
            'market', NEW.market,
            'symbol', NEW.symbol,
            'source', NEW.source,
            'supply_apy', NEW.supply_apy,
            'borrow_apy', NEW.borrow_apy,
            'utilization', NEW.utilization,
            'diff_changed', NEW.diff_changed
        )::text
    );
    RETURN NEW;
END;
$$;
DROP TRIGGER IF EXISTS notify_reserve_update ON kamino.reserve_updates;
CREATE TRIGGER notify_reserve_update
AFTER INSERT ON kamino.reserve_updates
FOR EACH ROW
EXECUTE FUNCTION kamino.notify_reserve_update();
CREATE OR REPLACE VIEW kamino.latest_reserve_updates AS
SELECT DISTINCT ON (reserve)
    observed_at,
    slot,
    reserve
FROM kamino.reserve_updates
ORDER BY reserve, observed_at DESC, slot DESC;
"#,
    )
    .execute(pool)
    .await?;
    Ok(())
}

async fn insert_reserve_update(
    pool: &sqlx::PgPool,
    reserve: &LocalReserve,
    slot: i64,
    supply_apy: f64,
    changed: bool,
) -> Result<()> {
    let changed_fields = if changed {
        vec!["supply_apy".to_owned()]
    } else {
        Vec::new()
    };
    let target = json!({
        "reserve": reserve.reserve.to_string(),
        "market": reserve.market.to_string(),
        "symbol": "USDC",
    });
    let snapshot = json!({
        "supply_apy": supply_apy,
        "borrow_apy": 0.0,
        "utilization": 0.0,
    });
    let diff = json!({
        "changed": changed,
        "changed_fields": changed_fields,
    });
    let record = json!({
        "kind": "reserve_update",
        "source": "local-validator",
        "target": target,
        "snapshot": snapshot,
        "diff": diff,
    });
    sqlx::query(
        r#"
        INSERT INTO kamino.reserve_updates (
            observed_at, slot, kind, source, reserve, market, market_name, symbol,
            liquidity_mint, mint_decimals, reserve_last_update_slot,
            reserve_last_update_stale, reserve_price_status, available_amount,
            borrowed_amount, borrowed_amount_sf, total_supply_amount, market_price_usd,
            market_price_last_updated_ts, cumulative_borrow_rate_bsf,
            total_supply_usd_estimate, total_borrow_usd_estimate, utilization,
            borrow_apr, supply_apr, borrow_apy, supply_apy, protocol_take_rate_pct,
            host_fixed_interest_rate_bps, diff_changed, changed_fields, diff_summary,
            diff, target, snapshot, record
        ) VALUES (
            now(), $1, 'reserve_update', 'local-validator', $2, $3, NULL, 'USDC',
            $4, $5, $1, FALSE, 0, 1000000.0,
            0.0, '0', 1000000.0, 1.0,
            0, '0',
            1000000.0, 0.0, 0.0,
            0.0, $6, 0.0, $6, 0,
            0, $7, $8, $9,
            $10, $11, $12, $13
        )
        "#,
    )
    .bind(slot)
    .bind(reserve.reserve.to_string())
    .bind(reserve.market.to_string())
    .bind(USDC_MINT.to_string())
    .bind(i32::from(USDC_DECIMALS))
    .bind(supply_apy)
    .bind(changed)
    .bind(changed_fields)
    .bind(if changed {
        "supply_apy changed"
    } else {
        "baseline"
    })
    .bind(diff)
    .bind(target)
    .bind(snapshot)
    .bind(record)
    .execute(pool)
    .await?;
    Ok(())
}

struct TokenBalances {
    vault_usdc: u64,
    main_collateral: u64,
    prime_collateral: u64,
    main_reserve_supply: u64,
    prime_reserve_supply: u64,
}

impl TokenBalances {
    fn fetch(rpc: &RpcClient, accounts: &LocalAccounts) -> Result<Self> {
        Ok(Self {
            vault_usdc: token_amount(rpc, accounts.main.vault_liquidity)?,
            main_collateral: token_amount(rpc, accounts.main.vault_collateral)?,
            prime_collateral: token_amount(rpc, accounts.prime.vault_collateral)?,
            main_reserve_supply: token_amount(rpc, accounts.main.reserve_liquidity_supply)?,
            prime_reserve_supply: token_amount(rpc, accounts.prime.reserve_liquidity_supply)?,
        })
    }

    fn assert_routed(&self) -> Result<()> {
        if self.vault_usdc != 0
            || self.main_collateral != 0
            || self.prime_collateral != AMOUNT
            || self.main_reserve_supply != 0
            || self.prime_reserve_supply != AMOUNT
        {
            bail!(
                "unexpected routed balances: vault_usdc={} main_collateral={} prime_collateral={} main_supply={} prime_supply={}",
                self.vault_usdc,
                self.main_collateral,
                self.prime_collateral,
                self.main_reserve_supply,
                self.prime_reserve_supply
            );
        }
        Ok(())
    }
}

fn token_amount(rpc: &RpcClient, pubkey: Pubkey) -> Result<u64> {
    let account: Account = rpc.get_account(&pubkey)?;
    let token = spl_token::state::Account::unpack(&account.data)
        .map_err(|error| anyhow!("unpack SPL token account {pubkey}: {error:?}"))?;
    Ok(token.amount)
}

async fn assert_db_submission(pool: &sqlx::PgPool) -> Result<()> {
    let row = sqlx::query(
        r#"
        SELECT status::text AS status, signature
        FROM loyal_yield.rebalance_decisions
        ORDER BY id DESC
        LIMIT 1
        "#,
    )
    .fetch_one(pool)
    .await?;
    let status: String = row.try_get("status")?;
    let signature: Option<String> = row.try_get("signature")?;
    if status != "submitted" || signature.is_none() {
        bail!("expected submitted decision with signature, got status={status} signature={signature:?}");
    }
    Ok(())
}

fn create_work_dir() -> Result<PathBuf> {
    let timestamp = SystemTime::now().duration_since(UNIX_EPOCH)?.as_millis();
    let path = PathBuf::from(format!("/private/tmp/loyal-same-mint-e2e-{timestamp}"));
    fs::create_dir_all(&path)?;
    Ok(path)
}

fn local_cli_keypair() -> Result<Keypair> {
    let home = std::env::var("HOME").context("HOME is not set")?;
    let keypair_path = PathBuf::from(home).join(".config/solana/id.json");
    read_keypair_file(&keypair_path).map_err(|error| {
        anyhow!(
            "read local Solana CLI keypair {}: {error}",
            keypair_path.display()
        )
    })
}

fn free_port() -> Result<u16> {
    let listener = TcpListener::bind("127.0.0.1:0")?;
    Ok(listener.local_addr()?.port())
}

fn validator_ports() -> Result<(u16, u16)> {
    let rpc_port = std::env::var(LOCAL_VALIDATOR_RPC_PORT_ENV)
        .ok()
        .map(|value| value.parse())
        .transpose()?
        .unwrap_or(DEFAULT_LOCAL_VALIDATOR_RPC_PORT);
    let faucet_port = std::env::var(LOCAL_VALIDATOR_FAUCET_PORT_ENV)
        .ok()
        .map(|value| value.parse())
        .transpose()?
        .unwrap_or(DEFAULT_LOCAL_VALIDATOR_FAUCET_PORT);
    if ports_available(rpc_port, faucet_port) {
        return Ok((rpc_port, faucet_port));
    }
    free_validator_ports()
}

fn ports_available(rpc_port: u16, faucet_port: u16) -> bool {
    let Some(ws_port) = rpc_port.checked_add(1) else {
        return false;
    };
    let Ok(_rpc) = TcpListener::bind(("0.0.0.0", rpc_port)) else {
        return false;
    };
    let Ok(_ws) = TcpListener::bind(("0.0.0.0", ws_port)) else {
        return false;
    };
    let Ok(_faucet) = TcpListener::bind(("0.0.0.0", faucet_port)) else {
        return false;
    };
    true
}

fn free_validator_ports() -> Result<(u16, u16)> {
    for _ in 0..100 {
        let rpc_probe = TcpListener::bind("0.0.0.0:0")?;
        let rpc_port = rpc_probe.local_addr()?.port();
        let Some(ws_port) = rpc_port.checked_add(1) else {
            continue;
        };
        let ws_probe = TcpListener::bind(("0.0.0.0", ws_port));
        if ws_probe.is_err() {
            continue;
        }
        let faucet_probe = TcpListener::bind("0.0.0.0:0")?;
        let faucet_port = faucet_probe.local_addr()?.port();
        if faucet_port != rpc_port && faucet_port != ws_port {
            return Ok((rpc_port, faucet_port));
        }
    }
    bail!("could not find free validator RPC/websocket/faucet ports")
}
