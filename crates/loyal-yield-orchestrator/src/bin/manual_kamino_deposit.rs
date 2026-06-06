use clap::Parser;
use loyal_yield_orchestrator::{
    build_kamino_deposit_sync_transaction, RpcRouteSubmitter, SameMintReserveTarget,
};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{pubkey::Pubkey, signature::read_keypair_file, signer::Signer};
use std::{fs, path::PathBuf, str::FromStr};

#[derive(Debug, Parser)]
#[command(about = "Manually deposit vault liquidity into a Kamino reserve through Squads sync")]
struct Cli {
    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: String,
    #[arg(short = 'k', long = "keypair")]
    keypair: PathBuf,
    #[arg(long)]
    settings: String,
    #[arg(long)]
    vault: String,
    #[arg(long, default_value_t = 0)]
    vault_index: u8,
    #[arg(long)]
    amount_raw: u64,
    #[arg(long)]
    target_json: Option<String>,
    #[arg(long)]
    target_file: Option<PathBuf>,
    #[arg(long)]
    submit: bool,
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    let target = load_target(&cli)?;
    let rpc = RpcClient::new(cli.rpc_url);
    let signer = read_keypair_file(&cli.keypair)
        .map_err(|err| format!("failed to read keypair {}: {err}", cli.keypair.display()))?;
    let settings = Pubkey::from_str(&cli.settings)?;
    let vault = Pubkey::from_str(&cli.vault)?;
    let transaction = build_kamino_deposit_sync_transaction(
        settings,
        signer.pubkey(),
        cli.vault_index,
        vault,
        &target,
        cli.amount_raw,
    )?;
    let submitter = RpcRouteSubmitter::new(&rpc);
    let simulation =
        submitter.simulate_instructions(&[transaction.instruction.clone()], &signer)?;

    let mut report = serde_json::json!({
        "transaction": transaction.report,
        "simulation": simulation.report,
        "submitted": false,
    });
    if cli.submit {
        if !simulation.ok {
            return Err(format!("simulation failed: {}", simulation.report).into());
        }
        let submission = submitter.submit_and_confirm(&[transaction.instruction], &signer)?;
        report["submitted"] = serde_json::json!(true);
        report["submission"] = submission.report;
    }

    println!("{}", serde_json::to_string_pretty(&report)?);
    Ok(())
}

fn load_target(cli: &Cli) -> Result<SameMintReserveTarget, Box<dyn std::error::Error>> {
    let json = if let Some(target_json) = &cli.target_json {
        target_json.clone()
    } else if let Some(target_file) = &cli.target_file {
        fs::read_to_string(target_file)?
    } else {
        return Err("pass --target-json or --target-file".into());
    };
    Ok(serde_json::from_str(&json)?)
}
