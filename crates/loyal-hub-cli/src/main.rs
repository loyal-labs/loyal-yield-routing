use std::{
    collections::HashSet,
    env,
    path::{Path, PathBuf},
};

use anyhow::{anyhow, bail, Context, Result};
use clap::{ArgAction, Parser, Subcommand, ValueEnum};
use loyal_actions::{
    derive_loyal_hub_config_for_program, derive_loyal_hub_lane_authority_for_program,
    derive_loyal_hub_lane_inventory_account_for_program, hub_rebalance,
    loyal_hub_initialize_config_instruction_for_program,
    loyal_hub_set_max_fee_instruction_for_program, loyal_hub_set_paused_instruction_for_program,
    loyal_hub_swap_exact_in_instruction_for_program,
    loyal_hub_withdraw_inventory_instruction_for_program, LoyalHubRebalanceTransfer,
    LoyalHubSwapExactIn, LOYAL_HUB_SWAP_PROGRAM_ID,
};
use serde::Serialize;
use solana_account_decoder::UiAccount;
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcSimulateTransactionAccountsConfig, RpcSimulateTransactionConfig},
};
use solana_program::program_pack::Pack;
use solana_sdk::{
    account::Account,
    commitment_config::CommitmentConfig,
    instruction::Instruction,
    pubkey::Pubkey,
    signature::{read_keypair_file, Keypair, Signer},
    transaction::Transaction,
};
use spl_token::state::{Account as SplTokenAccount, Mint};

const MAINNET_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const DEVNET_RPC_URL: &str = "https://api.devnet.solana.com";
const TESTNET_RPC_URL: &str = "https://api.testnet.solana.com";
const LOCAL_RPC_URL: &str = "http://localhost:8899";

#[derive(Debug, Parser)]
#[command(author, version, about)]
struct Cli {
    #[arg(
        short = 'u',
        long = "url",
        global = true,
        env = "SOLANA_RPC_URL",
        default_value = "d",
        help = "RPC URL or cluster alias: m/mainnet, d/devnet, t/testnet, l/local"
    )]
    url: String,

    #[arg(
        short = 'k',
        long = "keypair",
        global = true,
        value_name = "PATH",
        help = "Fee-payer keypair path. Defaults to ~/.config/solana/id.json for transactions"
    )]
    keypair: Option<PathBuf>,

    #[arg(
        long = "signer",
        global = true,
        value_name = "PATH",
        help = "Additional signer keypair path; repeat for multi-signer instructions"
    )]
    signer: Vec<PathBuf>,

    #[arg(
        long,
        global = true,
        default_value_t = LOYAL_HUB_SWAP_PROGRAM_ID,
        help = "Loyal Hub program id used for instruction program_id and PDA derivation"
    )]
    program_id: Pubkey,

    #[arg(long, global = true, default_value_t = CommitmentArg::Confirmed)]
    commitment: CommitmentArg,

    #[arg(
        long,
        global = true,
        action = ArgAction::SetTrue,
        help = "Simulate the transaction and print fee, compute, log, and balance estimates"
    )]
    simulate: bool,

    #[arg(long, global = true, action = ArgAction::SetTrue)]
    json: bool,

    #[command(subcommand)]
    command: Command,
}

#[derive(Clone, Copy, Debug, ValueEnum)]
enum CommitmentArg {
    Processed,
    Confirmed,
    Finalized,
}

impl std::fmt::Display for CommitmentArg {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Processed => formatter.write_str("processed"),
            Self::Confirmed => formatter.write_str("confirmed"),
            Self::Finalized => formatter.write_str("finalized"),
        }
    }
}

impl CommitmentArg {
    fn config(self) -> CommitmentConfig {
        match self {
            Self::Processed => CommitmentConfig::processed(),
            Self::Confirmed => CommitmentConfig::confirmed(),
            Self::Finalized => CommitmentConfig::finalized(),
        }
    }
}

#[derive(Debug, Subcommand)]
enum Command {
    #[command(about = "Display current hub config and lane inventory balances")]
    State,

    #[command(about = "Initialize the hub config account")]
    InitializeConfig {
        #[arg(long, help = "Hub admin signer. Defaults to the fee-payer pubkey")]
        admin: Option<Pubkey>,
        #[arg(long)]
        hub_authorizer: Pubkey,
        #[arg(long)]
        inventory_rebalancer: Pubkey,
        #[arg(long)]
        max_fee_bps: u16,
        #[arg(long, default_value_t = false)]
        paused: bool,
        #[arg(long)]
        lane_count: u8,
        #[arg(long = "mint", required = true)]
        allowed_mints: Vec<Pubkey>,
    },

    #[command(about = "Set the hub maximum fee in basis points")]
    SetMaxFee {
        max_fee_bps: u16,
        #[arg(long, help = "Hub admin signer. Defaults to the fee-payer pubkey")]
        admin: Option<Pubkey>,
    },

    #[command(about = "Pause or unpause hub swaps")]
    SetPaused {
        paused: bool,
        #[arg(long, help = "Hub admin signer. Defaults to the fee-payer pubkey")]
        admin: Option<Pubkey>,
    },

    #[command(about = "Withdraw inventory from a hub lane")]
    WithdrawInventory {
        #[arg(long, help = "Hub admin signer. Defaults to the fee-payer pubkey")]
        admin: Option<Pubkey>,
        #[arg(long)]
        destination_token_account: Pubkey,
        #[arg(long)]
        mint: Pubkey,
        #[arg(long)]
        amount: u64,
        #[arg(long)]
        lane_id: u8,
    },

    #[command(about = "Execute SwapExactIn against one hub lane")]
    SwapExactIn {
        #[arg(long, help = "User vault signer. Defaults to the fee-payer pubkey")]
        user_vault: Option<Pubkey>,
        #[arg(long)]
        user_input_token_account: Pubkey,
        #[arg(long)]
        user_output_token_account: Pubkey,
        #[arg(long)]
        input_mint: Pubkey,
        #[arg(long)]
        output_mint: Pubkey,
        #[arg(long, help = "Hub authorizer signer. Defaults to the fee-payer pubkey")]
        hub_authorizer: Option<Pubkey>,
        #[arg(long)]
        amount_in: u64,
        #[arg(long)]
        amount_out: u64,
        #[arg(long)]
        min_out: u64,
        #[arg(long)]
        max_fee_bps: u16,
        #[arg(long)]
        lane_id: u8,
    },

    #[command(about = "Rebalance lane inventory; groups transfers by mint")]
    RebalanceInventory {
        #[arg(
            long,
            help = "Inventory rebalancer signer. Defaults to the fee-payer pubkey"
        )]
        inventory_rebalancer: Option<Pubkey>,
        #[arg(long, help = "Default mint for transfers that omit mint:<PUBKEY>")]
        mint: Option<Pubkey>,
        #[arg(
            long = "transfer",
            value_name = "FIELD:VALUE",
            num_args = 1..=4,
            action = ArgAction::Append,
            required = true,
            help = "Transfer group, e.g. --transfer from_lane_id:0 to_lane_id:1 raw_token_amount:1000"
        )]
        transfers: Vec<String>,
    },
}

#[derive(Debug)]
struct SignerSet {
    keypairs: Vec<Keypair>,
    pubkeys: HashSet<Pubkey>,
}

impl SignerSet {
    fn fee_payer(&self) -> Pubkey {
        self.keypairs[0].pubkey()
    }

    fn contains(&self, pubkey: &Pubkey) -> bool {
        self.pubkeys.contains(pubkey)
    }

    fn signer_refs(&self) -> Vec<&dyn Signer> {
        self.keypairs
            .iter()
            .map(|keypair| keypair as &dyn Signer)
            .collect()
    }
}

#[derive(Clone, Debug)]
struct DecodedHubConfig {
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: Vec<Pubkey>,
}

#[derive(Clone, Debug)]
struct AccountSnapshot {
    lamports: u64,
    owner: Pubkey,
    data: Vec<u8>,
}

#[derive(Clone, Debug)]
struct TokenSnapshot {
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
}

#[derive(Debug, Serialize)]
struct HubStateReport {
    initialized: bool,
    rpc_url: String,
    program_id: String,
    config_account: String,
    config_lamports: Option<u64>,
    admin: Option<String>,
    hub_authorizer: Option<String>,
    inventory_rebalancer: Option<String>,
    max_fee_bps: Option<u16>,
    paused: Option<bool>,
    lane_count: Option<u8>,
    allowed_mints: Vec<String>,
    lanes: Vec<LaneStateReport>,
}

#[derive(Debug, Serialize)]
struct LaneStateReport {
    lane_id: u8,
    authority: String,
    inventory: Vec<InventoryStateReport>,
}

#[derive(Debug, Serialize)]
struct InventoryStateReport {
    mint: String,
    account: String,
    exists: bool,
    amount: Option<u64>,
    decimals: Option<u8>,
    ui_amount: Option<String>,
}

#[derive(Debug, Serialize)]
struct TransactionReport {
    mode: &'static str,
    rpc_url: String,
    program_id: String,
    fee_payer: String,
    signature: Option<String>,
    fee_lamports: Option<u64>,
    simulation: Option<SimulationReport>,
}

#[derive(Debug, Serialize)]
struct SimulationReport {
    err: Option<String>,
    units_consumed: Option<u64>,
    loaded_accounts_data_size: Option<u32>,
    replacement_blockhash: Option<String>,
    balance_changes: Vec<BalanceChangeReport>,
    logs: Vec<String>,
}

#[derive(Debug, Serialize)]
struct BalanceChangeReport {
    address: String,
    lamports_before: Option<u64>,
    lamports_after: Option<u64>,
    lamports_delta: Option<i128>,
    token_mint: Option<String>,
    token_owner: Option<String>,
    token_amount_before: Option<u64>,
    token_amount_after: Option<u64>,
    token_amount_delta: Option<i128>,
}

fn main() -> Result<()> {
    let cli = Cli::parse();
    let rpc_url = resolve_rpc_url(&cli.url);
    let rpc = RpcClient::new_with_commitment(rpc_url.clone(), cli.commitment.config());

    match &cli.command {
        Command::State => {
            let state = fetch_hub_state(&rpc, &rpc_url, cli.program_id)?;
            output_state(&state, cli.json)
        }
        command => {
            let signers = load_signers(&cli)?;
            let fee_payer = signers.fee_payer();
            let instructions = build_instructions(command, cli.program_id, fee_payer)?;
            require_signers(&instructions, fee_payer, &signers)?;
            let report = execute_or_simulate(
                &rpc,
                &rpc_url,
                cli.program_id,
                &instructions,
                &signers,
                cli.simulate,
            )?;
            output_transaction_report(&report, cli.json)
        }
    }
}

fn resolve_rpc_url(value: &str) -> String {
    match value.trim().to_ascii_lowercase().as_str() {
        "m" | "mainnet" | "mainnet-beta" => MAINNET_RPC_URL.to_owned(),
        "d" | "devnet" => DEVNET_RPC_URL.to_owned(),
        "t" | "testnet" => TESTNET_RPC_URL.to_owned(),
        "l" | "local" | "localhost" | "localnet" => LOCAL_RPC_URL.to_owned(),
        other => other.to_owned(),
    }
}

fn load_signers(cli: &Cli) -> Result<SignerSet> {
    let mut paths = Vec::new();
    paths.push(
        cli.keypair
            .clone()
            .or_else(default_keypair_path)
            .context("a keypair is required for transaction commands; pass -k/--keypair")?,
    );
    paths.extend(cli.signer.iter().cloned());

    let mut keypairs = Vec::new();
    let mut pubkeys = HashSet::new();
    for path in paths {
        let keypair = read_keypair_file(expand_tilde(&path))
            .map_err(|error| anyhow!("read keypair {}: {error}", path.display()))?;
        if pubkeys.insert(keypair.pubkey()) {
            keypairs.push(keypair);
        }
    }

    if keypairs.is_empty() {
        bail!("at least one signer keypair is required");
    }

    Ok(SignerSet { keypairs, pubkeys })
}

fn default_keypair_path() -> Option<PathBuf> {
    env::var_os("HOME").map(|home| {
        PathBuf::from(home)
            .join(".config")
            .join("solana")
            .join("id.json")
    })
}

fn expand_tilde(path: &Path) -> PathBuf {
    let Some(path_string) = path.to_str() else {
        return path.to_owned();
    };
    if path_string == "~" {
        return env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or(path.to_owned());
    }
    if let Some(rest) = path_string.strip_prefix("~/") {
        if let Some(home) = env::var_os("HOME") {
            return PathBuf::from(home).join(rest);
        }
    }
    path.to_owned()
}

fn build_instructions(
    command: &Command,
    program_id: Pubkey,
    fee_payer: Pubkey,
) -> Result<Vec<Instruction>> {
    let instructions = match command {
        Command::State => unreachable!("state is read-only"),
        Command::InitializeConfig {
            admin,
            hub_authorizer,
            inventory_rebalancer,
            max_fee_bps,
            paused,
            lane_count,
            allowed_mints,
        } => vec![loyal_hub_initialize_config_instruction_for_program(
            program_id,
            admin.unwrap_or(fee_payer),
            admin.unwrap_or(fee_payer),
            *hub_authorizer,
            *inventory_rebalancer,
            *max_fee_bps,
            *paused,
            *lane_count,
            allowed_mints,
        )?],
        Command::SetMaxFee { max_fee_bps, admin } => {
            vec![loyal_hub_set_max_fee_instruction_for_program(
                program_id,
                admin.unwrap_or(fee_payer),
                *max_fee_bps,
            )?]
        }
        Command::SetPaused { paused, admin } => vec![loyal_hub_set_paused_instruction_for_program(
            program_id,
            admin.unwrap_or(fee_payer),
            *paused,
        )],
        Command::WithdrawInventory {
            admin,
            destination_token_account,
            mint,
            amount,
            lane_id,
        } => vec![loyal_hub_withdraw_inventory_instruction_for_program(
            program_id,
            admin.unwrap_or(fee_payer),
            *destination_token_account,
            *mint,
            *amount,
            *lane_id,
        )],
        Command::SwapExactIn {
            user_vault,
            user_input_token_account,
            user_output_token_account,
            input_mint,
            output_mint,
            hub_authorizer,
            amount_in,
            amount_out,
            min_out,
            max_fee_bps,
            lane_id,
        } => vec![loyal_hub_swap_exact_in_instruction_for_program(
            program_id,
            user_vault.unwrap_or(fee_payer),
            *user_input_token_account,
            *user_output_token_account,
            *input_mint,
            *output_mint,
            hub_authorizer.unwrap_or(fee_payer),
            LoyalHubSwapExactIn {
                amount_in: *amount_in,
                amount_out: *amount_out,
                min_out: *min_out,
                max_fee_bps: *max_fee_bps,
                lane_id: *lane_id,
            },
        )],
        Command::RebalanceInventory {
            inventory_rebalancer,
            mint,
            transfers,
        } => {
            let transfers = parse_rebalance_transfers(transfers, *mint)?;
            hub_rebalance().instructions_for_program(
                program_id,
                inventory_rebalancer.unwrap_or(fee_payer),
                transfers,
            )?
        }
    };

    Ok(instructions)
}

fn parse_rebalance_transfers(
    transfer_tokens: &[String],
    default_mint: Option<Pubkey>,
) -> Result<Vec<LoyalHubRebalanceTransfer>> {
    let groups = split_rebalance_transfer_groups(transfer_tokens)?;
    groups
        .iter()
        .map(|group| parse_rebalance_transfer(group, default_mint))
        .collect()
}

fn split_rebalance_transfer_groups(transfer_tokens: &[String]) -> Result<Vec<Vec<String>>> {
    let mut groups = Vec::new();
    let mut current = Vec::new();
    let mut seen_fields = HashSet::new();

    for token in flatten_transfer_tokens(transfer_tokens) {
        let key = transfer_field_key(&token)?;
        if !current.is_empty()
            && (seen_fields.contains(&key)
                || (is_transfer_start_key(&key) && has_complete_transfer_fields(&seen_fields)))
        {
            groups.push(std::mem::take(&mut current));
            seen_fields.clear();
        }
        seen_fields.insert(key);
        current.push(token);
    }

    if !current.is_empty() {
        groups.push(current);
    }
    if groups.is_empty() {
        bail!("at least one --transfer is required");
    }

    Ok(groups)
}

fn flatten_transfer_tokens(values: &[String]) -> Vec<String> {
    values
        .iter()
        .flat_map(|value| value.split(|ch: char| ch.is_ascii_whitespace() || ch == ','))
        .filter(|value| !value.trim().is_empty())
        .map(str::to_owned)
        .collect()
}

fn transfer_field_key(token: &str) -> Result<String> {
    let (key, _) = token
        .split_once(':')
        .or_else(|| token.split_once('='))
        .with_context(|| format!("transfer field must be key:value, got {token}"))?;
    Ok(key.replace('-', "_").to_ascii_lowercase())
}

fn is_transfer_start_key(key: &str) -> bool {
    matches!(key, "from_lane_id" | "from_lane" | "from" | "mint")
}

fn has_complete_transfer_fields(fields: &HashSet<String>) -> bool {
    fields
        .iter()
        .any(|field| matches!(field.as_str(), "from_lane_id" | "from_lane" | "from"))
        && fields
            .iter()
            .any(|field| matches!(field.as_str(), "to_lane_id" | "to_lane" | "to"))
        && fields
            .iter()
            .any(|field| matches!(field.as_str(), "raw_token_amount" | "amount"))
}

fn parse_rebalance_transfer(
    group: &[String],
    default_mint: Option<Pubkey>,
) -> Result<LoyalHubRebalanceTransfer> {
    let mut from_lane_id = None;
    let mut to_lane_id = None;
    let mut amount = None;
    let mut mint = default_mint;

    for token in group
        .iter()
        .flat_map(|value| value.split(|ch: char| ch.is_ascii_whitespace() || ch == ','))
        .filter(|value| !value.trim().is_empty())
    {
        let (key, value) = token
            .split_once(':')
            .or_else(|| token.split_once('='))
            .with_context(|| format!("transfer field must be key:value, got {token}"))?;
        let key = key.replace('-', "_").to_ascii_lowercase();
        match key.as_str() {
            "from_lane_id" | "from_lane" | "from" => {
                from_lane_id = Some(parse_transfer_u8(value, "from_lane_id")?);
            }
            "to_lane_id" | "to_lane" | "to" => {
                to_lane_id = Some(parse_transfer_u8(value, "to_lane_id")?);
            }
            "raw_token_amount" | "amount" => {
                amount = Some(parse_transfer_u64(value, "raw_token_amount")?);
            }
            "mint" => mint = Some(value.parse().context("parse transfer mint")?),
            _ => bail!("unknown transfer field {key}"),
        }
    }

    Ok(LoyalHubRebalanceTransfer {
        mint: mint.context("transfer mint is required; pass --mint or mint:<PUBKEY>")?,
        from_lane_id: from_lane_id.context("transfer from_lane_id is required")?,
        to_lane_id: to_lane_id.context("transfer to_lane_id is required")?,
        amount: amount.context("transfer raw_token_amount is required")?,
    })
}

fn parse_transfer_u8(value: &str, field: &str) -> Result<u8> {
    value
        .parse()
        .with_context(|| format!("parse {field} as u8"))
}

fn parse_transfer_u64(value: &str, field: &str) -> Result<u64> {
    value
        .parse()
        .with_context(|| format!("parse {field} as u64"))
}

fn require_signers(
    instructions: &[Instruction],
    fee_payer: Pubkey,
    signers: &SignerSet,
) -> Result<()> {
    let mut required = vec![fee_payer];
    for instruction in instructions {
        for account in &instruction.accounts {
            if account.is_signer && !required.contains(&account.pubkey) {
                required.push(account.pubkey);
            }
        }
    }

    let missing = required
        .into_iter()
        .filter(|pubkey| !signers.contains(pubkey))
        .collect::<Vec<_>>();
    if !missing.is_empty() {
        let missing = missing
            .iter()
            .map(ToString::to_string)
            .collect::<Vec<_>>()
            .join(", ");
        bail!("missing signer keypair(s): {missing}; pass them with -k/--keypair or --signer");
    }

    Ok(())
}

fn execute_or_simulate(
    rpc: &RpcClient,
    rpc_url: &str,
    program_id: Pubkey,
    instructions: &[Instruction],
    signers: &SignerSet,
    simulate: bool,
) -> Result<TransactionReport> {
    let fee_payer = signers.fee_payer();
    let blockhash = rpc.get_latest_blockhash().context("get latest blockhash")?;
    let signer_refs = signers.signer_refs();
    let tx =
        Transaction::new_signed_with_payer(instructions, Some(&fee_payer), &signer_refs, blockhash);
    let fee_lamports = rpc.get_fee_for_message(&tx.message).ok();

    if simulate {
        let simulation = simulate_transaction(rpc, instructions, &tx, fee_payer)?;
        return Ok(TransactionReport {
            mode: "simulate",
            rpc_url: rpc_url.to_owned(),
            program_id: program_id.to_string(),
            fee_payer: fee_payer.to_string(),
            signature: None,
            fee_lamports,
            simulation: Some(simulation),
        });
    }

    let signature = rpc
        .send_and_confirm_transaction(&tx)
        .context("send and confirm transaction")?;

    Ok(TransactionReport {
        mode: "execute",
        rpc_url: rpc_url.to_owned(),
        program_id: program_id.to_string(),
        fee_payer: fee_payer.to_string(),
        signature: Some(signature.to_string()),
        fee_lamports,
        simulation: None,
    })
}

fn simulate_transaction(
    rpc: &RpcClient,
    instructions: &[Instruction],
    tx: &Transaction,
    fee_payer: Pubkey,
) -> Result<SimulationReport> {
    let watched_accounts = watched_accounts(instructions, fee_payer);
    let before_accounts = rpc
        .get_multiple_accounts(&watched_accounts)
        .context("fetch pre-simulation accounts")?
        .into_iter()
        .map(|account| account.map(AccountSnapshot::from))
        .collect::<Vec<_>>();

    let config = RpcSimulateTransactionConfig {
        sig_verify: false,
        replace_recent_blockhash: false,
        commitment: Some(rpc.commitment()),
        accounts: Some(RpcSimulateTransactionAccountsConfig {
            encoding: Some(solana_account_decoder::UiAccountEncoding::Base64),
            addresses: watched_accounts.iter().map(ToString::to_string).collect(),
        }),
        ..RpcSimulateTransactionConfig::default()
    };
    let response = rpc
        .simulate_transaction_with_config(tx, config)
        .context("simulate transaction")?;
    let value = response.value;

    let after_accounts = match value.accounts {
        Some(accounts) => accounts
            .iter()
            .map(|account| account.as_ref().map(AccountSnapshot::try_from).transpose())
            .collect::<Result<Vec<_>>>()?,
        None => Vec::new(),
    };
    let balance_changes = balance_changes(&watched_accounts, &before_accounts, &after_accounts);

    Ok(SimulationReport {
        err: value.err.map(|err| format!("{err:?}")),
        units_consumed: value.units_consumed,
        loaded_accounts_data_size: value.loaded_accounts_data_size,
        replacement_blockhash: value
            .replacement_blockhash
            .map(|blockhash| blockhash.blockhash),
        balance_changes,
        logs: value.logs.unwrap_or_default(),
    })
}

fn watched_accounts(instructions: &[Instruction], fee_payer: Pubkey) -> Vec<Pubkey> {
    let mut watched = vec![fee_payer];
    for instruction in instructions {
        for account in &instruction.accounts {
            if !watched.contains(&account.pubkey) {
                watched.push(account.pubkey);
            }
        }
    }
    watched
}

fn balance_changes(
    addresses: &[Pubkey],
    before_accounts: &[Option<AccountSnapshot>],
    after_accounts: &[Option<AccountSnapshot>],
) -> Vec<BalanceChangeReport> {
    addresses
        .iter()
        .enumerate()
        .filter_map(|(index, address)| {
            let before = before_accounts.get(index).and_then(Option::as_ref);
            let after = after_accounts.get(index).and_then(Option::as_ref);
            let before_token = before.and_then(decode_token_snapshot);
            let after_token = after.and_then(decode_token_snapshot);
            let lamports_delta = delta_u64(before.map(|a| a.lamports), after.map(|a| a.lamports));
            let token_amount_delta = delta_u64(
                before_token.as_ref().map(|token| token.amount),
                after_token.as_ref().map(|token| token.amount),
            );

            if lamports_delta == Some(0)
                && token_amount_delta == Some(0)
                && before_token.is_none()
                && after_token.is_none()
            {
                return None;
            }

            let token_mint = after_token
                .as_ref()
                .or(before_token.as_ref())
                .map(|token| token.mint.to_string());
            let token_owner = after_token
                .as_ref()
                .or(before_token.as_ref())
                .map(|token| token.owner.to_string());

            Some(BalanceChangeReport {
                address: address.to_string(),
                lamports_before: before.map(|account| account.lamports),
                lamports_after: after.map(|account| account.lamports),
                lamports_delta,
                token_mint,
                token_owner,
                token_amount_before: before_token.as_ref().map(|token| token.amount),
                token_amount_after: after_token.as_ref().map(|token| token.amount),
                token_amount_delta,
            })
        })
        .collect()
}

fn delta_u64(before: Option<u64>, after: Option<u64>) -> Option<i128> {
    match (before, after) {
        (Some(before), Some(after)) => Some(after as i128 - before as i128),
        (None, Some(after)) => Some(after as i128),
        (Some(before), None) => Some(-(before as i128)),
        (None, None) => None,
    }
}

fn fetch_hub_state(rpc: &RpcClient, rpc_url: &str, program_id: Pubkey) -> Result<HubStateReport> {
    let config_account = derive_loyal_hub_config_for_program(program_id);
    let hub_accounts = rpc
        .get_multiple_accounts_with_commitment(&[program_id, config_account], rpc.commitment())
        .with_context(|| {
            format!("fetch hub program {program_id} and config account {config_account}")
        })?
        .value;
    let program_account = hub_accounts.first().and_then(Option::as_ref);
    require_hub_program_account(rpc_url, program_id, program_account)?;

    let Some(account) = hub_accounts.get(1).and_then(Option::as_ref) else {
        return Ok(HubStateReport {
            initialized: false,
            rpc_url: rpc_url.to_owned(),
            program_id: program_id.to_string(),
            config_account: config_account.to_string(),
            config_lamports: None,
            admin: None,
            hub_authorizer: None,
            inventory_rebalancer: None,
            max_fee_bps: None,
            paused: None,
            lane_count: None,
            allowed_mints: Vec::new(),
            lanes: Vec::new(),
        });
    };
    if account.owner != program_id {
        bail!(
            "hub config account owner mismatch: got {}, expected {}",
            account.owner,
            program_id
        );
    }
    let config = decode_hub_config(&account.data)?;
    let mint_decimals = fetch_mint_decimals(rpc, &config.allowed_mints)?;

    let mut inventory_pubkeys = Vec::new();
    for lane_id in 0..config.lane_count {
        for mint in &config.allowed_mints {
            inventory_pubkeys.push(derive_loyal_hub_lane_inventory_account_for_program(
                program_id, *mint, lane_id,
            ));
        }
    }
    let inventory_accounts = rpc
        .get_multiple_accounts(&inventory_pubkeys)
        .context("fetch hub inventory accounts")?;

    let mut account_index = 0;
    let mut lanes = Vec::new();
    for lane_id in 0..config.lane_count {
        let mut inventory = Vec::new();
        for (mint_index, mint) in config.allowed_mints.iter().enumerate() {
            let inventory_account = inventory_pubkeys[account_index];
            let account = inventory_accounts
                .get(account_index)
                .and_then(Option::as_ref);
            let amount = account
                .and_then(|account| SplTokenAccount::unpack(&account.data).ok())
                .map(|account| account.amount);
            let decimals = mint_decimals[mint_index];
            inventory.push(InventoryStateReport {
                mint: mint.to_string(),
                account: inventory_account.to_string(),
                exists: account.is_some(),
                amount,
                decimals,
                ui_amount: amount
                    .zip(decimals)
                    .map(|(amount, decimals)| format_token_amount(amount, decimals)),
            });
            account_index += 1;
        }
        lanes.push(LaneStateReport {
            lane_id,
            authority: derive_loyal_hub_lane_authority_for_program(program_id, lane_id).to_string(),
            inventory,
        });
    }

    Ok(HubStateReport {
        initialized: true,
        rpc_url: rpc_url.to_owned(),
        program_id: program_id.to_string(),
        config_account: config_account.to_string(),
        config_lamports: Some(account.lamports),
        admin: Some(config.admin.to_string()),
        hub_authorizer: Some(config.hub_authorizer.to_string()),
        inventory_rebalancer: Some(config.inventory_rebalancer.to_string()),
        max_fee_bps: Some(config.max_fee_bps),
        paused: Some(config.paused),
        lane_count: Some(config.lane_count),
        allowed_mints: config
            .allowed_mints
            .iter()
            .map(ToString::to_string)
            .collect(),
        lanes,
    })
}

fn require_hub_program_account(
    rpc_url: &str,
    program_id: Pubkey,
    account: Option<&Account>,
) -> Result<()> {
    let Some(account) = account else {
        bail!("hub program account {program_id} does not exist on {rpc_url}");
    };
    if !account.executable {
        bail!(
            "hub program account {program_id} exists on {rpc_url} but is not executable; owner {}",
            account.owner
        );
    }
    Ok(())
}

fn fetch_mint_decimals(rpc: &RpcClient, mints: &[Pubkey]) -> Result<Vec<Option<u8>>> {
    let accounts = rpc
        .get_multiple_accounts(mints)
        .context("fetch mint accounts")?;
    Ok(accounts
        .iter()
        .map(|account| {
            account
                .as_ref()
                .and_then(|account| Mint::unpack(&account.data).ok())
                .map(|mint| mint.decimals)
        })
        .collect())
}

fn decode_hub_config(data: &[u8]) -> Result<DecodedHubConfig> {
    if data.len() != loyal_hub_abi::CONFIG_ACCOUNT_MAX_LEN {
        bail!(
            "invalid hub config data length: got {}, expected {}",
            data.len(),
            loyal_hub_abi::CONFIG_ACCOUNT_MAX_LEN
        );
    }
    let magic_range = loyal_hub_abi::config_account::MAGIC_OFFSET
        ..loyal_hub_abi::config_account::MAGIC_OFFSET + loyal_hub_abi::config_account::MAGIC_LEN;
    if data.get(magic_range) != Some(loyal_hub_abi::CONFIG_MAGIC.as_slice()) {
        bail!("hub config account magic mismatch");
    }

    let mint_count = read_u8_at(data, loyal_hub_abi::config_account::MINT_COUNT_OFFSET)?;
    if mint_count == 0 || mint_count as usize > loyal_hub_abi::MAX_ALLOWED_MINTS {
        bail!("invalid hub config mint_count {mint_count}");
    }
    let lane_count = read_u8_at(data, loyal_hub_abi::config_account::LANE_COUNT_OFFSET)?;
    if lane_count == 0 {
        bail!("invalid hub config lane_count 0");
    }
    let mut allowed_mints = Vec::with_capacity(mint_count as usize);
    for index in 0..mint_count as usize {
        let offset = loyal_hub_abi::config_account::ALLOWED_MINT_OFFSET
            + (index * loyal_hub_abi::config_account::ALLOWED_MINT_ITEM_LEN);
        let mint = read_pubkey_at(data, offset)?;
        if allowed_mints.contains(&mint) {
            bail!("duplicate allowed mint {mint}");
        }
        allowed_mints.push(mint);
    }

    Ok(DecodedHubConfig {
        admin: read_pubkey_at(data, loyal_hub_abi::config_account::ADMIN_OFFSET)?,
        hub_authorizer: read_pubkey_at(data, loyal_hub_abi::config_account::HUB_AUTHORIZER_OFFSET)?,
        inventory_rebalancer: read_pubkey_at(
            data,
            loyal_hub_abi::config_account::INVENTORY_REBALANCER_OFFSET,
        )?,
        max_fee_bps: read_u16_at(data, loyal_hub_abi::config_account::MAX_FEE_BPS_OFFSET)?,
        paused: read_u8_at(data, loyal_hub_abi::config_account::PAUSED_OFFSET)? != 0,
        lane_count,
        allowed_mints,
    })
}

fn read_pubkey_at(data: &[u8], offset: usize) -> Result<Pubkey> {
    let bytes = data
        .get(offset..offset + 32)
        .with_context(|| format!("read pubkey at offset {offset}"))?;
    let mut pubkey = [0_u8; 32];
    pubkey.copy_from_slice(bytes);
    Ok(Pubkey::new_from_array(pubkey))
}

fn read_u16_at(data: &[u8], offset: usize) -> Result<u16> {
    let bytes = data
        .get(offset..offset + 2)
        .with_context(|| format!("read u16 at offset {offset}"))?;
    Ok(u16::from_le_bytes([bytes[0], bytes[1]]))
}

fn read_u8_at(data: &[u8], offset: usize) -> Result<u8> {
    data.get(offset)
        .copied()
        .with_context(|| format!("read u8 at offset {offset}"))
}

fn decode_token_snapshot(account: &AccountSnapshot) -> Option<TokenSnapshot> {
    if account.owner != spl_token::id() {
        return None;
    }
    SplTokenAccount::unpack(&account.data)
        .ok()
        .map(|token_account| TokenSnapshot {
            mint: token_account.mint,
            owner: token_account.owner,
            amount: token_account.amount,
        })
}

fn format_token_amount(amount: u64, decimals: u8) -> String {
    if decimals == 0 {
        return amount.to_string();
    }
    let scale = 10_u64.saturating_pow(decimals as u32);
    let whole = amount / scale;
    let fractional = amount % scale;
    let mut fractional = format!("{fractional:0width$}", width = decimals as usize);
    while fractional.ends_with('0') {
        fractional.pop();
    }
    if fractional.is_empty() {
        whole.to_string()
    } else {
        format!("{whole}.{fractional}")
    }
}

fn output_state(report: &HubStateReport, json: bool) -> Result<()> {
    if json {
        print_json(report)
    } else {
        println!("Hub program: {}", report.program_id);
        println!("RPC URL: {}", report.rpc_url);
        if !report.initialized {
            println!("Config: {} (not initialized)", report.config_account);
            println!("Initialized: false");
            return Ok(());
        }
        println!(
            "Config: {} ({} lamports)",
            report.config_account,
            report.config_lamports.unwrap_or_default()
        );
        println!("Initialized: true");
        println!("Admin: {}", report.admin.as_deref().unwrap_or("-"));
        println!(
            "Hub authorizer: {}",
            report.hub_authorizer.as_deref().unwrap_or("-")
        );
        println!(
            "Inventory rebalancer: {}",
            report.inventory_rebalancer.as_deref().unwrap_or("-")
        );
        println!("Max fee bps: {}", report.max_fee_bps.unwrap_or_default());
        println!("Paused: {}", report.paused.unwrap_or_default());
        println!("Lane count: {}", report.lane_count.unwrap_or_default());
        println!("Allowed mints:");
        for mint in &report.allowed_mints {
            println!("  {mint}");
        }
        println!("Inventory:");
        for lane in &report.lanes {
            println!("  Lane {} authority {}", lane.lane_id, lane.authority);
            for inventory in &lane.inventory {
                let amount = inventory
                    .amount
                    .map(|amount| amount.to_string())
                    .unwrap_or_else(|| "missing".to_owned());
                let ui_amount = inventory.ui_amount.as_deref().unwrap_or("-");
                println!(
                    "    mint {} account {} amount {} ui {}",
                    inventory.mint, inventory.account, amount, ui_amount
                );
            }
        }
        Ok(())
    }
}

fn output_transaction_report(report: &TransactionReport, json: bool) -> Result<()> {
    if json {
        print_json(report)
    } else {
        println!("Mode: {}", report.mode);
        println!("RPC URL: {}", report.rpc_url);
        println!("Hub program: {}", report.program_id);
        println!("Fee payer: {}", report.fee_payer);
        if let Some(fee_lamports) = report.fee_lamports {
            println!("Estimated transaction fee: {fee_lamports} lamports");
        }
        if let Some(signature) = &report.signature {
            println!("Signature: {signature}");
        }
        if let Some(simulation) = &report.simulation {
            println!(
                "Simulation error: {}",
                simulation.err.as_deref().unwrap_or("none")
            );
            if let Some(units) = simulation.units_consumed {
                println!("Compute units consumed: {units}");
            }
            if let Some(size) = simulation.loaded_accounts_data_size {
                println!("Loaded accounts data size: {size}");
            }
            println!("Balance changes:");
            for change in &simulation.balance_changes {
                print_balance_change(change);
            }
            if !simulation.logs.is_empty() {
                println!("Logs:");
                for log in &simulation.logs {
                    println!("  {log}");
                }
            }
        }
        Ok(())
    }
}

fn print_balance_change(change: &BalanceChangeReport) {
    println!("  {}", change.address);
    if let Some(delta) = change.lamports_delta {
        println!(
            "    lamports: {:?} -> {:?} ({delta:+})",
            change.lamports_before, change.lamports_after
        );
    }
    if let Some(delta) = change.token_amount_delta {
        println!(
            "    token: {:?} owner {:?} amount {:?} -> {:?} ({delta:+})",
            change.token_mint,
            change.token_owner,
            change.token_amount_before,
            change.token_amount_after
        );
    }
}

fn print_json<T: Serialize>(value: &T) -> Result<()> {
    serde_json::to_writer_pretty(std::io::stdout(), value).context("write JSON")?;
    println!();
    Ok(())
}

impl From<Account> for AccountSnapshot {
    fn from(account: Account) -> Self {
        Self {
            lamports: account.lamports,
            owner: account.owner,
            data: account.data,
        }
    }
}

impl TryFrom<&UiAccount> for AccountSnapshot {
    type Error = anyhow::Error;

    fn try_from(account: &UiAccount) -> Result<Self> {
        let owner = account
            .owner
            .parse()
            .context("parse simulated account owner")?;
        let data = account
            .data
            .decode()
            .context("decode simulated account data")?;
        Ok(Self {
            lamports: account.lamports,
            owner,
            data,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn resolves_solana_cluster_aliases() {
        assert_eq!(resolve_rpc_url("m"), MAINNET_RPC_URL);
        assert_eq!(resolve_rpc_url("devnet"), DEVNET_RPC_URL);
        assert_eq!(resolve_rpc_url("t"), TESTNET_RPC_URL);
        assert_eq!(resolve_rpc_url("local"), LOCAL_RPC_URL);
        assert_eq!(resolve_rpc_url("http://localhost:8899"), LOCAL_RPC_URL);
    }

    #[test]
    fn parses_space_separated_rebalance_transfer_with_default_mint() {
        let mint = Pubkey::new_unique();
        let transfer = parse_rebalance_transfer(
            &[
                "from_lane_id:0".to_owned(),
                "to_lane_id:1".to_owned(),
                "raw_token_amount:1000".to_owned(),
            ],
            Some(mint),
        )
        .unwrap();

        assert_eq!(transfer.mint, mint);
        assert_eq!(transfer.from_lane_id, 0);
        assert_eq!(transfer.to_lane_id, 1);
        assert_eq!(transfer.amount, 1000);
    }

    #[test]
    fn parses_quoted_rebalance_transfer_with_mint() {
        let mint = Pubkey::new_unique();
        let transfer = parse_rebalance_transfer(
            &[format!(
                "mint:{mint},from_lane_id:2,to_lane_id:3,raw_token_amount:42"
            )],
            None,
        )
        .unwrap();

        assert_eq!(transfer.mint, mint);
        assert_eq!(transfer.from_lane_id, 2);
        assert_eq!(transfer.to_lane_id, 3);
        assert_eq!(transfer.amount, 42);
    }

    #[test]
    fn clap_accepts_repeated_space_separated_transfer_groups() {
        let mint = Pubkey::new_unique();
        let cli = Cli::try_parse_from([
            "loyal-hub",
            "rebalance-inventory",
            "--mint",
            &mint.to_string(),
            "--transfer",
            "from_lane_id:0",
            "to_lane_id:1",
            "raw_token_amount:1000",
            "--transfer",
            "from_lane_id:1",
            "to_lane_id:0",
            "raw_token_amount:500",
        ])
        .unwrap();

        let Command::RebalanceInventory { transfers, .. } = cli.command else {
            panic!("expected rebalance command");
        };
        let parsed = parse_rebalance_transfers(&transfers, Some(mint)).unwrap();

        assert_eq!(parsed.len(), 2);
        assert_eq!(parsed[0].amount, 1000);
        assert_eq!(parsed[1].from_lane_id, 1);
        assert_eq!(parsed[1].amount, 500);
    }

    #[test]
    fn formats_token_amounts_without_trailing_zeroes() {
        assert_eq!(format_token_amount(1_230_000, 6), "1.23");
        assert_eq!(format_token_amount(1_000_000, 6), "1");
        assert_eq!(format_token_amount(42, 0), "42");
    }

    #[test]
    fn missing_hub_program_account_is_an_error() {
        let program_id = Pubkey::new_unique();
        let error = require_hub_program_account("https://example.invalid", program_id, None)
            .unwrap_err()
            .to_string();

        assert!(error.contains("does not exist"));
        assert!(error.contains(&program_id.to_string()));
    }

    #[test]
    fn non_executable_hub_program_account_is_an_error() {
        let program_id = Pubkey::new_unique();
        let account = Account {
            lamports: 1,
            data: Vec::new(),
            owner: Pubkey::new_unique(),
            executable: false,
            rent_epoch: 0,
        };
        let error =
            require_hub_program_account("https://example.invalid", program_id, Some(&account))
                .unwrap_err()
                .to_string();

        assert!(error.contains("not executable"));
    }

    #[test]
    fn executable_hub_program_account_is_valid() {
        let account = Account {
            lamports: 1,
            data: Vec::new(),
            owner: Pubkey::new_unique(),
            executable: true,
            rent_epoch: 0,
        };

        require_hub_program_account(
            "https://example.invalid",
            Pubkey::new_unique(),
            Some(&account),
        )
        .unwrap();
    }

    #[test]
    fn builds_swap_instruction_with_custom_program_id() {
        let program_id = Pubkey::new_unique();
        let input_mint = Pubkey::new_unique();
        let output_mint = Pubkey::new_unique();
        let ix = loyal_hub_swap_exact_in_instruction_for_program(
            program_id,
            Pubkey::new_unique(),
            Pubkey::new_unique(),
            Pubkey::new_unique(),
            input_mint,
            output_mint,
            Pubkey::new_unique(),
            LoyalHubSwapExactIn {
                amount_in: 1,
                amount_out: 1,
                min_out: 1,
                max_fee_bps: 50,
                lane_id: 4,
            },
        );

        assert_eq!(ix.program_id, program_id);
        assert_eq!(
            ix.accounts[0].pubkey,
            derive_loyal_hub_config_for_program(program_id)
        );
        assert_eq!(
            ix.accounts[4].pubkey,
            derive_loyal_hub_lane_inventory_account_for_program(program_id, input_mint, 4)
        );
        assert_eq!(
            ix.accounts[8].pubkey,
            derive_loyal_hub_lane_authority_for_program(program_id, 4)
        );
    }
}
