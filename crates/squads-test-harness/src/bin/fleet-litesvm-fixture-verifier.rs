use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use litesvm::LiteSVM;
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};
use solana_sdk::{account::Account, pubkey::Pubkey};
use squads_test_harness::{
    KAMINO_LEND_PROGRAM_ID, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE, KAMINO_PRIME_MARKET,
    KAMINO_PRIME_USDC_RESERVE, SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_MINT,
};
use std::{
    collections::BTreeSet,
    env,
    error::Error,
    fs,
    path::{Path, PathBuf},
    str::FromStr,
};

const MAINNET_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Manifest {
    schema_version: u8,
    kind: String,
    source: Source,
    roots: Roots,
    accounts: Vec<ManifestAccount>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct Source {
    cluster: String,
    genesis_hash: String,
    commitment: String,
    minimum_context_slot: u64,
}

#[derive(Debug, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Roots {
    squads_program: String,
    kamino_lend_program: String,
    kamino_farms_program: String,
    main_market: String,
    prime_market: String,
    main_usdc_reserve: String,
    prime_usdc_reserve: String,
    usdc_mint: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ManifestAccount {
    address: String,
    file: String,
    owner: String,
    executable: bool,
    lamports: String,
    data_length: usize,
    data_sha256: String,
    file_sha256: String,
}

#[derive(Debug, Deserialize)]
struct SolanaAccountFile {
    pubkey: String,
    account: SolanaAccountJson,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SolanaAccountJson {
    lamports: u64,
    data: (String, String),
    owner: String,
    executable: bool,
    rent_epoch: u64,
    space: usize,
}

fn sha256(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

fn exact_path(base: &Path, relative: &str) -> Result<PathBuf, Box<dyn Error>> {
    if relative.is_empty() || relative.starts_with('/') || relative.contains("..") {
        return Err(format!("unsafe fixture account path: {relative}").into());
    }
    let path = base.join(relative).canonicalize()?;
    if !path.starts_with(base) {
        return Err(format!("fixture account path escapes fixture root: {relative}").into());
    }
    Ok(path)
}

fn assert_root(name: &str, actual: &str, expected: Pubkey) -> Result<(), Box<dyn Error>> {
    if actual != expected.to_string() {
        return Err(format!("fixture root {name} is {actual}, expected {expected}").into());
    }
    Ok(())
}

fn run(manifest_path: &Path) -> Result<serde_json::Value, Box<dyn Error>> {
    let manifest_path = manifest_path.canonicalize()?;
    let fixture_root = manifest_path
        .parent()
        .ok_or("manifest has no parent directory")?
        .canonicalize()?;
    let manifest: Manifest = serde_json::from_slice(&fs::read(&manifest_path)?)?;
    if manifest.schema_version != 1 || manifest.kind != "loyal-fleet-mainnet-clone" {
        return Err("unsupported fixture manifest contract".into());
    }
    if manifest.source.cluster != "mainnet-beta"
        || manifest.source.genesis_hash != MAINNET_GENESIS_HASH
        || manifest.source.commitment != "finalized"
        || manifest.source.minimum_context_slot == 0
    {
        return Err("fixture is not a finalized canonical Mainnet capture".into());
    }
    assert_root(
        "squadsProgram",
        &manifest.roots.squads_program,
        SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )?;
    assert_root(
        "kaminoLendProgram",
        &manifest.roots.kamino_lend_program,
        KAMINO_LEND_PROGRAM_ID,
    )?;
    assert_root(
        "kaminoFarmsProgram",
        &manifest.roots.kamino_farms_program,
        loyal_actions::KAMINO_FARMS_PROGRAM_ID,
    )?;
    assert_root(
        "mainMarket",
        &manifest.roots.main_market,
        KAMINO_MAIN_MARKET,
    )?;
    assert_root(
        "primeMarket",
        &manifest.roots.prime_market,
        KAMINO_PRIME_MARKET,
    )?;
    assert_root(
        "mainUsdcReserve",
        &manifest.roots.main_usdc_reserve,
        KAMINO_MAIN_USDC_RESERVE,
    )?;
    assert_root(
        "primeUsdcReserve",
        &manifest.roots.prime_usdc_reserve,
        KAMINO_PRIME_USDC_RESERVE,
    )?;
    assert_root("usdcMint", &manifest.roots.usdc_mint, USDC_MINT)?;

    let root_addresses = [
        &manifest.roots.squads_program,
        &manifest.roots.kamino_lend_program,
        &manifest.roots.kamino_farms_program,
        &manifest.roots.main_market,
        &manifest.roots.prime_market,
        &manifest.roots.main_usdc_reserve,
        &manifest.roots.prime_usdc_reserve,
        &manifest.roots.usdc_mint,
    ];
    let addresses = manifest
        .accounts
        .iter()
        .map(|account| account.address.as_str())
        .collect::<BTreeSet<_>>();
    if addresses.len() != manifest.accounts.len() {
        return Err("fixture contains duplicate account addresses".into());
    }
    for root in root_addresses {
        if !addresses.contains(root.as_str()) {
            return Err(format!("fixture root account is absent: {root}").into());
        }
    }

    let mut svm = LiteSVM::new();
    let mut total_data_bytes = 0usize;
    for entry in &manifest.accounts {
        let path = exact_path(&fixture_root, &entry.file)?;
        let raw_file = fs::read(&path)?;
        if sha256(&raw_file) != entry.file_sha256 {
            return Err(format!("fixture file digest mismatch for {}", entry.address).into());
        }
        let account_file: SolanaAccountFile = serde_json::from_slice(&raw_file)?;
        if account_file.pubkey != entry.address || account_file.account.data.1 != "base64" {
            return Err(format!("invalid Solana account file for {}", entry.address).into());
        }
        let data = BASE64_STANDARD.decode(&account_file.account.data.0)?;
        let lamports = entry.lamports.parse::<u64>()?;
        if account_file.account.owner != entry.owner
            || account_file.account.executable != entry.executable
            || account_file.account.lamports != lamports
            || account_file.account.space != entry.data_length
            || data.len() != entry.data_length
            || sha256(&data) != entry.data_sha256
        {
            return Err(format!("manifest/account mismatch for {}", entry.address).into());
        }
        let pubkey = Pubkey::from_str(&entry.address)?;
        let owner = Pubkey::from_str(&entry.owner)?;
        svm.set_account(
            pubkey,
            Account {
                lamports,
                data: data.clone(),
                owner,
                executable: entry.executable,
                rent_epoch: account_file.account.rent_epoch,
            },
        )?;
        let loaded = svm
            .get_account(&pubkey)
            .ok_or_else(|| format!("LiteSVM did not retain {}", entry.address))?;
        if loaded.lamports != lamports
            || loaded.data != data
            || loaded.owner != owner
            || loaded.executable != entry.executable
            || loaded.rent_epoch != account_file.account.rent_epoch
        {
            return Err(format!("LiteSVM read-back mismatch for {}", entry.address).into());
        }
        total_data_bytes += data.len();
    }

    Ok(json!({
        "schemaVersion": 1,
        "kind": "loyal-fleet-litesvm-fixture-verification",
        "engine": "LiteSVM",
        "fixture": {
            "cluster": "mainnet-beta",
            "commitment": "finalized",
            "sourceSlot": manifest.source.minimum_context_slot,
            "manifestAccountCount": manifest.accounts.len(),
            "loadedAccountCount": manifest.accounts.len(),
            "readBackMatchedAccountCount": manifest.accounts.len(),
            "totalDataBytes": total_data_bytes,
            "allDataHashesMatched": true,
            "allFileHashesMatched": true,
            "allRootsPresent": true,
            "rootsMatchLoyalActions": true,
        },
        "roots": manifest.roots,
        "boundary": {
            "networkAccessed": false,
            "rpcUsed": false,
            "databaseUsed": false,
            "transactionsSentToNetwork": false,
            "privateKeysLoaded": false,
        }
    }))
}

fn main() -> Result<(), Box<dyn Error>> {
    let mut args = env::args().skip(1);
    let manifest = match (args.next().as_deref(), args.next(), args.next()) {
        (Some("--manifest"), Some(path), None) => PathBuf::from(path),
        _ => return Err("usage: fleet-litesvm-fixture-verifier --manifest PATH".into()),
    };
    println!("{}", serde_json::to_string_pretty(&run(&manifest)?)?);
    Ok(())
}
