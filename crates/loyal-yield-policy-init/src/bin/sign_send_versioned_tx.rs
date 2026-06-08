use std::{fs, path::PathBuf};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use clap::Parser;
use serde_json::Value;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    signature::{read_keypair_file, Signer},
    transaction::VersionedTransaction,
};

#[derive(Debug, Parser)]
struct Cli {
    #[arg(long)]
    rpc_url: String,
    #[arg(short = 'k', long)]
    keypair: PathBuf,
    #[arg(long)]
    swap_json: PathBuf,
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let cli = Cli::parse();
    let json: Value = serde_json::from_str(&fs::read_to_string(&cli.swap_json)?)?;
    let transaction = json
        .get("swapTransaction")
        .and_then(Value::as_str)
        .ok_or("swap JSON is missing swapTransaction")?;
    let bytes = BASE64_STANDARD.decode(transaction)?;
    let unsigned: VersionedTransaction = bincode::deserialize(&bytes)?;
    let payer = read_keypair_file(&cli.keypair)
        .map_err(|error| format!("failed to read {}: {error}", cli.keypair.display()))?;
    let signed = VersionedTransaction::try_new(unsigned.message, &[&payer])?;
    let rpc = RpcClient::new(cli.rpc_url);
    let signature = rpc.send_and_confirm_transaction(&signed)?;
    println!(
        "{}",
        serde_json::to_string_pretty(&serde_json::json!({
            "signature": signature.to_string(),
            "signer": payer.pubkey().to_string(),
        }))?
    );
    Ok(())
}
