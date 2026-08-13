use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine};
use borsh::BorshDeserialize;
use loyal_actions::{
    compile_squads_inner_instruction, derive_action_account,
    derive_classic_associated_token_account, derive_kamino_user_metadata,
    execute_sync_transaction_instruction, SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_MINT,
    YIELD_ROUTE_WITHDRAW_ACTION_SEED,
};
use serde::Serialize;
use serde_json::json;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    pubkey::Pubkey,
    rent::Rent,
    signature::{read_keypair_file, Keypair, Signer},
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use spl_token::{
    solana_program::{program_option::COption, program_pack::Pack},
    state::{Account as TokenAccount, AccountState},
};
use squads_test_harness::{
    create_squads_smart_account_instruction_with_treasury, derive_squads_program_config,
    derive_squads_settings, derive_squads_vault,
};
use std::{
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    str::FromStr,
    thread::sleep,
    time::Duration,
};

const DEFAULT_WALLET_USDC_RAW: u64 = 100_000_000;
const DEFAULT_WALLET_AIRDROP_LAMPORTS: u64 = 100_000_000_000;
const DEFAULT_POLICY_AIRDROP_LAMPORTS: u64 = 10_000_000_000;
const DEFAULT_VAULT_LAMPORTS: u64 = 1_000_000_000;

#[derive(BorshDeserialize)]
struct ProgramConfigWire {
    _discriminator: [u8; 8],
    smart_account_index: u128,
    _authority: Pubkey,
    _smart_account_creation_fee: u64,
    treasury: Pubkey,
    _reserved: [u8; 64],
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct SolanaCliAccountEnvelope {
    pubkey: String,
    account: SolanaCliAccount,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct SolanaCliAccount {
    lamports: u64,
    data: (String, &'static str),
    owner: String,
    executable: bool,
    rent_epoch: u64,
    space: usize,
}

enum Command {
    PrepareGenesis {
        wallet_keypair: PathBuf,
        output: PathBuf,
        amount_raw: u64,
    },
    Setup {
        rpc_url: String,
        wallet_keypair: PathBuf,
        policy_keypair: PathBuf,
        vault_index: u8,
        vault_lamports: u64,
    },
}

fn main() -> Result<(), Box<dyn Error>> {
    match parse_args(env::args().skip(1))? {
        Command::PrepareGenesis {
            wallet_keypair,
            output,
            amount_raw,
        } => prepare_genesis(&wallet_keypair, &output, amount_raw),
        Command::Setup {
            rpc_url,
            wallet_keypair,
            policy_keypair,
            vault_index,
            vault_lamports,
        } => setup_local_chain(
            &rpc_url,
            &wallet_keypair,
            &policy_keypair,
            vault_index,
            vault_lamports,
        ),
    }
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Command, Box<dyn Error>> {
    let mut args = args.into_iter();
    let command = args.next().ok_or_else(usage)?;
    let mut wallet_keypair = None;
    let mut policy_keypair = None;
    let mut output = None;
    let mut rpc_url = None;
    let mut amount_raw = DEFAULT_WALLET_USDC_RAW;
    let mut vault_index = 1u8;
    let mut vault_lamports = DEFAULT_VAULT_LAMPORTS;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--wallet-keypair" => {
                wallet_keypair = Some(PathBuf::from(next_value(&mut args, &arg)?))
            }
            "--policy-keypair" => {
                policy_keypair = Some(PathBuf::from(next_value(&mut args, &arg)?))
            }
            "--output" => output = Some(PathBuf::from(next_value(&mut args, &arg)?)),
            "--rpc-url" => rpc_url = Some(next_value(&mut args, &arg)?),
            "--amount-raw" => amount_raw = next_value(&mut args, &arg)?.parse()?,
            "--vault-index" => vault_index = next_value(&mut args, &arg)?.parse()?,
            "--vault-lamports" => vault_lamports = next_value(&mut args, &arg)?.parse()?,
            "--help" | "-h" => return Err(usage().into()),
            other => return Err(format!("unknown argument {other}\n{}", usage()).into()),
        }
    }
    let wallet_keypair = wallet_keypair.ok_or("--wallet-keypair is required")?;
    match command.as_str() {
        "prepare-genesis" => Ok(Command::PrepareGenesis {
            wallet_keypair,
            output: output.ok_or("prepare-genesis requires --output")?,
            amount_raw,
        }),
        "setup" => Ok(Command::Setup {
            rpc_url: rpc_url.ok_or("setup requires --rpc-url")?,
            wallet_keypair,
            policy_keypair: policy_keypair.ok_or("setup requires --policy-keypair")?,
            vault_index,
            vault_lamports,
        }),
        _ => Err(usage().into()),
    }
}

fn next_value(
    args: &mut impl Iterator<Item = String>,
    flag: &str,
) -> Result<String, Box<dyn Error>> {
    args.next()
        .ok_or_else(|| format!("{flag} requires a value").into())
}

fn usage() -> String {
    "Usage:\n  fleet-local-chain-setup prepare-genesis --wallet-keypair FILE --output ACCOUNT.json [--amount-raw N]\n  fleet-local-chain-setup setup --rpc-url http://127.0.0.1:PORT --wallet-keypair FILE --policy-keypair FILE [--vault-index N] [--vault-lamports N]".to_owned()
}

fn load_keypair(path: &Path) -> Result<Keypair, Box<dyn Error>> {
    read_keypair_file(path)
        .map_err(|_| format!("failed to load ephemeral keypair file {}", path.display()).into())
}

fn prepare_genesis(
    wallet_path: &Path,
    output: &Path,
    amount_raw: u64,
) -> Result<(), Box<dyn Error>> {
    if amount_raw == 0 {
        return Err("--amount-raw must be positive".into());
    }
    let wallet = load_keypair(wallet_path)?;
    let token_address = derive_classic_associated_token_account(wallet.pubkey(), USDC_MINT);
    let token_state = TokenAccount {
        mint: USDC_MINT,
        owner: wallet.pubkey(),
        amount: amount_raw,
        delegate: COption::None,
        state: AccountState::Initialized,
        is_native: COption::None,
        delegated_amount: 0,
        close_authority: COption::None,
    };
    let mut data = vec![0u8; TokenAccount::LEN];
    TokenAccount::pack(token_state, &mut data)?;
    let envelope = SolanaCliAccountEnvelope {
        pubkey: token_address.to_string(),
        account: SolanaCliAccount {
            lamports: Rent::default().minimum_balance(data.len()),
            data: (BASE64_STANDARD.encode(&data), "base64"),
            owner: spl_token::id().to_string(),
            executable: false,
            rent_epoch: 0,
            space: data.len(),
        },
    };
    if let Some(parent) = output.parent() {
        fs::create_dir_all(parent)?;
    }
    fs::write(output, format!("{}\n", serde_json::to_string(&envelope)?))?;
    println!(
        "{}",
        serde_json::to_string(&json!({
            "status": "PASS",
            "kind": "local-only-test-owned-token-account",
            "address": token_address.to_string(),
            "owner": wallet.pubkey().to_string(),
            "mint": USDC_MINT.to_string(),
            "amountRaw": amount_raw.to_string(),
            "accountFile": output,
            "containsKeyMaterial": false,
        }))?
    );
    Ok(())
}

fn setup_local_chain(
    rpc_url: &str,
    wallet_path: &Path,
    policy_path: &Path,
    vault_index: u8,
    vault_lamports: u64,
) -> Result<(), Box<dyn Error>> {
    validate_loopback_rpc(rpc_url)?;
    if vault_lamports == 0 {
        return Err("--vault-lamports must be positive".into());
    }
    let wallet = load_keypair(wallet_path)?;
    let policy = load_keypair(policy_path)?;
    if wallet.pubkey() == policy.pubkey() {
        return Err("ephemeral wallet and policy signers must differ".into());
    }
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let genesis = rpc.get_genesis_hash()?;
    loyal_solana_env::rpc_safety::validate_rpc_genesis_hash("localnet", genesis)?;

    airdrop_and_confirm(&rpc, &wallet.pubkey(), DEFAULT_WALLET_AIRDROP_LAMPORTS)?;
    airdrop_and_confirm(&rpc, &policy.pubkey(), DEFAULT_POLICY_AIRDROP_LAMPORTS)?;

    let program_config_address = derive_squads_program_config();
    let program_config_account = rpc.get_account(&program_config_address)?;
    if program_config_account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return Err("cloned Squads ProgramConfig has the wrong owner".into());
    }
    let program_config = ProgramConfigWire::try_from_slice(&program_config_account.data)
        .map_err(|_| "cloned Squads ProgramConfig has an unsupported layout")?;
    let settings_seed = program_config
        .smart_account_index
        .checked_add(1)
        .ok_or("Squads settings seed overflow")?;
    let (settings, _) = derive_squads_settings(settings_seed);
    let create = create_squads_smart_account_instruction_with_treasury(
        wallet.pubkey(),
        wallet.pubkey(),
        settings_seed,
        program_config.treasury,
    );
    send_and_confirm(&rpc, &wallet, &[create])?;

    let settings_account = rpc.get_account(&settings)?;
    if settings_account.owner != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        return Err("created Settings account has the wrong owner".into());
    }
    let (vault, _) = derive_squads_vault(&settings, vault_index);
    let route_policy = derive_action_account(&settings, YIELD_ROUTE_WITHDRAW_ACTION_SEED).0;
    let setup_policy = derive_action_account(
        &settings,
        YIELD_ROUTE_WITHDRAW_ACTION_SEED.saturating_add(1),
    )
    .0;
    send_and_confirm(
        &rpc,
        &wallet,
        &[system_instruction::transfer(
            &wallet.pubkey(),
            &vault,
            vault_lamports,
        )],
    )?;

    let vault_user_metadata = derive_kamino_user_metadata(vault);
    if rpc
        .get_account_with_commitment(&vault_user_metadata, CommitmentConfig::confirmed())?
        .value
        .is_none()
    {
        let inner = klend_interface::instructions::init_user_metadata(
            klend_interface::instructions::InitUserMetadataAccounts {
                owner: vault,
                fee_payer: vault,
                user_metadata: vault_user_metadata,
                referrer_user_metadata: None,
            },
            Pubkey::default(),
        );
        let mut transaction_accounts = Vec::new();
        let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner);
        let initialize_metadata = execute_sync_transaction_instruction(
            settings,
            wallet.pubkey(),
            vault_index,
            vec![compiled],
            transaction_accounts,
        );
        send_and_confirm(&rpc, &wallet, &[initialize_metadata])?;
    }
    let metadata_account = rpc.get_account(&vault_user_metadata)?;
    if metadata_account.owner != klend_interface::KLEND_PROGRAM_ID {
        return Err("created vault user metadata has the wrong owner".into());
    }

    let wallet_usdc = derive_classic_associated_token_account(wallet.pubkey(), USDC_MINT);
    let wallet_usdc_account = rpc.get_account(&wallet_usdc)?;
    let wallet_usdc_state = TokenAccount::unpack(&wallet_usdc_account.data)?;
    if wallet_usdc_state.owner != wallet.pubkey() || wallet_usdc_state.mint != USDC_MINT {
        return Err("local-only wallet USDC account does not match the ephemeral wallet".into());
    }

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": "PASS",
            "cluster": "localnet",
            "genesisHash": genesis.to_string(),
            "settingsSeed": settings_seed.to_string(),
            "settings": settings.to_string(),
            "vaultIndex": vault_index,
            "vault": vault.to_string(),
            "vaultLamports": rpc.get_balance(&vault)?.to_string(),
            "wallet": wallet.pubkey().to_string(),
            "policy": policy.pubkey().to_string(),
            "routePolicySeed": YIELD_ROUTE_WITHDRAW_ACTION_SEED.to_string(),
            "routePolicy": route_policy.to_string(),
            "setupPolicySeed": YIELD_ROUTE_WITHDRAW_ACTION_SEED.saturating_add(1).to_string(),
            "setupPolicy": setup_policy.to_string(),
            "walletUsdc": wallet_usdc.to_string(),
            "walletUsdcAmountRaw": wallet_usdc_state.amount.to_string(),
            "vaultUserMetadata": vault_user_metadata.to_string(),
            "vaultUserMetadataOwner": metadata_account.owner.to_string(),
            "productionKeyLoaded": false,
        }))?
    );
    Ok(())
}

fn validate_loopback_rpc(rpc_url: &str) -> Result<(), Box<dyn Error>> {
    let authority = rpc_url
        .strip_prefix("http://")
        .ok_or("local RPC must use http://127.0.0.1:PORT")?;
    if authority.contains(['/', '?', '#', '@']) {
        return Err("local RPC must not contain credentials, paths, queries, or fragments".into());
    }
    let (host, port) = authority
        .rsplit_once(':')
        .ok_or("local RPC must include an explicit port")?;
    if host != "127.0.0.1" || u16::from_str(port)? < 1024 {
        return Err("local RPC must use 127.0.0.1 and a non-privileged port".into());
    }
    Ok(())
}

fn airdrop_and_confirm(
    rpc: &RpcClient,
    recipient: &Pubkey,
    lamports: u64,
) -> Result<(), Box<dyn Error>> {
    let signature = rpc.request_airdrop(recipient, lamports)?;
    for _ in 0..100 {
        if rpc.confirm_transaction(&signature)? {
            return Ok(());
        }
        sleep(Duration::from_millis(50));
    }
    Err("local airdrop did not confirm".into())
}

fn send_and_confirm(
    rpc: &RpcClient,
    payer: &Keypair,
    instructions: &[solana_sdk::instruction::Instruction],
) -> Result<(), Box<dyn Error>> {
    let blockhash = rpc.get_latest_blockhash()?;
    let transaction = Transaction::new_signed_with_payer(
        instructions,
        Some(&payer.pubkey()),
        &[payer],
        blockhash,
    );
    rpc.send_and_confirm_transaction(&transaction)?;
    Ok(())
}
