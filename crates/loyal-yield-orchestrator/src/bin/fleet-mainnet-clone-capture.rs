use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use borsh::BorshDeserialize;
use chrono::Utc;
use loyal_actions::{
    KAMINO_FARMS_PROGRAM_ID, KAMINO_FIGURE_MARKET, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE,
    KAMINO_PRIME_USDC_RESERVE, SQUADS_SMART_ACCOUNT_PROGRAM_ID, USDC_MINT,
};
use loyal_yield_orchestrator::{
    derive_shared_market_catalog, load_finalized_kamino_reserve_catalog, SupportedKaminoReserve,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use solana_account_decoder_client_types::UiAccountEncoding;
use solana_client::{rpc_client::RpcClient, rpc_config::RpcAccountInfoConfig};
use solana_sdk::{
    account::Account, commitment_config::CommitmentConfig, native_loader, pubkey::Pubkey, sysvar,
};
use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    fs,
    path::PathBuf,
    str::FromStr,
};

const RPC_ENV: &str = "FLEET_FIXTURE_MAINNET_RPC_URL";
const MAINNET_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
const SQUADS_PROGRAM_CONFIG_SEED: &[u8] = b"program_config";
const GET_MULTIPLE_ACCOUNTS_LIMIT: usize = 100;
const UPGRADEABLE_LOADER_ID: &str = "BPFLoaderUpgradeab1e11111111111111111111111";

#[derive(BorshDeserialize)]
struct ProgramConfigWire {
    _discriminator: [u8; 8],
    _smart_account_index: u128,
    _authority: Pubkey,
    _smart_account_creation_fee: u64,
    treasury: Pubkey,
    _reserved: [u8; 64],
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct AccountEnvelope {
    pubkey: String,
    account: AccountFile,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct AccountFile {
    lamports: u64,
    data: (String, &'static str),
    owner: String,
    executable: bool,
    rent_epoch: u64,
    space: usize,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ManifestAccount {
    address: String,
    file: String,
    context_slot: u64,
    owner: String,
    executable: bool,
    lamports: String,
    data_length: usize,
    data_sha256: String,
    file_sha256: String,
    purposes: Vec<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Source {
    cluster: &'static str,
    genesis_hash: &'static str,
    commitment: &'static str,
    minimum_context_slot: u64,
    captured_at_utc: String,
}

#[derive(Serialize)]
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

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct LocalOnlyAccounts {
    created_after_validator_start: Vec<&'static str>,
    fabricated_at_genesis: Vec<&'static str>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Manifest {
    schema_version: u8,
    kind: &'static str,
    source: Source,
    roots: Roots,
    accounts: Vec<ManifestAccount>,
    local_only_accounts: LocalOnlyAccounts,
}

fn main() -> Result<(), Box<dyn Error>> {
    let output = parse_output(env::args().skip(1))?;
    let rpc_url = env::var(RPC_ENV).map_err(|_| {
        format!("{RPC_ENV} must name an explicit unauthenticated HTTPS Mainnet RPC endpoint")
    })?;
    validate_public_rpc_url(&rpc_url)?;
    if output.exists() {
        return Err(format!(
            "refusing to overwrite existing fixture directory {}",
            output.display()
        )
        .into());
    }

    let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::finalized());
    let genesis = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to verify the explicitly supplied RPC genesis")?;
    if genesis.to_string() != MAINNET_GENESIS_HASH {
        return Err("fixture capture RPC is not canonical Mainnet".into());
    }

    let now = Utc::now();
    let supported = vec![
        SupportedKaminoReserve {
            market: KAMINO_MAIN_MARKET.to_string(),
            liquidity_mint: USDC_MINT.to_string(),
            reserve: KAMINO_MAIN_USDC_RESERVE.to_string(),
            market_name: Some("Main".to_owned()),
            symbol: Some("USDC".to_owned()),
            updated_at: now,
        },
        SupportedKaminoReserve {
            market: KAMINO_FIGURE_MARKET.to_string(),
            liquidity_mint: USDC_MINT.to_string(),
            reserve: KAMINO_PRIME_USDC_RESERVE.to_string(),
            market_name: Some("Prime".to_owned()),
            symbol: Some("USDC".to_owned()),
            updated_at: now,
        },
    ];
    let reserves = load_finalized_kamino_reserve_catalog(&rpc, &supported)
        .map_err(|_| "failed to decode the finalized Mainnet reserve roots")?;
    let catalog = derive_shared_market_catalog(&reserves.reserves)?;
    let minimum_context_slot = reserves.source_slot;

    let program_config = Pubkey::find_program_address(
        &[SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0;
    let mut purposes = BTreeMap::<Pubkey, BTreeSet<String>>::new();
    for (address, purpose) in [
        (
            SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            "required-root:squads-program",
        ),
        (
            klend_interface::KLEND_PROGRAM_ID,
            "required-root:kamino-lend-program",
        ),
        (
            KAMINO_FARMS_PROGRAM_ID,
            "required-root:kamino-farms-program",
        ),
        (KAMINO_MAIN_MARKET, "required-root:main-market"),
        (KAMINO_FIGURE_MARKET, "required-root:prime-market"),
        (KAMINO_MAIN_USDC_RESERVE, "required-root:main-usdc-reserve"),
        (
            KAMINO_PRIME_USDC_RESERVE,
            "required-root:prime-usdc-reserve",
        ),
        (USDC_MINT, "required-root:usdc-mint"),
        (program_config, "squads-program-config"),
    ] {
        purposes
            .entry(address)
            .or_default()
            .insert(purpose.to_owned());
    }
    for entry in catalog.addresses {
        let address = Pubkey::from_str(&entry.address)?;
        purposes
            .entry(address)
            .or_default()
            .insert(format!("shared-market:{}", entry.account_role));
    }

    let first = fetch_accounts(
        &rpc,
        purposes.keys().copied().collect(),
        minimum_context_slot,
    )?;
    let config_account = first
        .get(&program_config)
        .and_then(|(account, _)| account.as_ref())
        .ok_or("finalized clone closure is missing Squads ProgramConfig")?;
    let config = ProgramConfigWire::try_from_slice(&config_account.data)
        .map_err(|_| "finalized Squads ProgramConfig has an unsupported layout")?;
    purposes
        .entry(config.treasury)
        .or_default()
        .insert("squads-program-config-treasury".to_owned());

    for (address, (account, _)) in &first {
        let Some(account) = account else { continue };
        if account.owner == upgradeable_loader_id() && account.executable {
            let program_data = upgradeable_program_data_address(account)?;
            purposes
                .entry(program_data)
                .or_default()
                .insert(format!("program-data:{address}"));
        }
    }

    let fetched = fetch_accounts(
        &rpc,
        purposes.keys().copied().collect(),
        minimum_context_slot,
    )?;
    let required = BTreeSet::from([
        SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        klend_interface::KLEND_PROGRAM_ID,
        KAMINO_FARMS_PROGRAM_ID,
        KAMINO_MAIN_MARKET,
        KAMINO_FIGURE_MARKET,
        KAMINO_MAIN_USDC_RESERVE,
        KAMINO_PRIME_USDC_RESERVE,
        USDC_MINT,
        program_config,
    ]);
    for address in &required {
        if fetched
            .get(address)
            .and_then(|(account, _)| account.as_ref())
            .is_none()
        {
            return Err(
                format!("finalized clone closure is missing required account {address}").into(),
            );
        }
    }

    fs::create_dir_all(output.join("accounts"))?;
    let mut manifest_accounts = Vec::new();
    for (address, purpose_set) in purposes {
        let Some((Some(account), context_slot)) = fetched.get(&address) else {
            continue;
        };
        if account.owner == native_loader::id() || account.owner == sysvar::id() {
            continue;
        }
        let relative_file = format!("accounts/{address}.json");
        let envelope = AccountEnvelope {
            pubkey: address.to_string(),
            account: AccountFile {
                lamports: account.lamports,
                data: (BASE64_STANDARD.encode(&account.data), "base64"),
                owner: account.owner.to_string(),
                executable: account.executable,
                rent_epoch: account.rent_epoch,
                space: account.data.len(),
            },
        };
        let raw = format!("{}\n", serde_json::to_string(&envelope)?);
        fs::write(output.join(&relative_file), raw.as_bytes())?;
        manifest_accounts.push(ManifestAccount {
            address: address.to_string(),
            file: relative_file,
            context_slot: *context_slot,
            owner: account.owner.to_string(),
            executable: account.executable,
            lamports: account.lamports.to_string(),
            data_length: account.data.len(),
            data_sha256: sha256(&account.data),
            file_sha256: sha256(raw.as_bytes()),
            purposes: purpose_set.into_iter().collect(),
        });
    }
    manifest_accounts.sort_by(|left, right| left.address.cmp(&right.address));

    let manifest = Manifest {
        schema_version: 1,
        kind: "loyal-fleet-mainnet-clone",
        source: Source {
            cluster: "mainnet-beta",
            genesis_hash: MAINNET_GENESIS_HASH,
            commitment: "finalized",
            minimum_context_slot,
            captured_at_utc: Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
        },
        roots: Roots {
            squads_program: SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string(),
            kamino_lend_program: klend_interface::KLEND_PROGRAM_ID.to_string(),
            kamino_farms_program: KAMINO_FARMS_PROGRAM_ID.to_string(),
            main_market: KAMINO_MAIN_MARKET.to_string(),
            prime_market: KAMINO_FIGURE_MARKET.to_string(),
            main_usdc_reserve: KAMINO_MAIN_USDC_RESERVE.to_string(),
            prime_usdc_reserve: KAMINO_PRIME_USDC_RESERVE.to_string(),
            usdc_mint: USDC_MINT.to_string(),
        },
        accounts: manifest_accounts,
        local_only_accounts: LocalOnlyAccounts {
            created_after_validator_start: vec![
                "ephemeral Squads settings and vault",
                "route and setup policies",
                "vault user metadata, token accounts, obligations, and farm user states",
                "reusable address lookup tables",
            ],
            fabricated_at_genesis: vec!["ephemeral wallet USDC token account"],
        },
    };
    fs::write(
        output.join("manifest.json"),
        format!("{}\n", serde_json::to_string_pretty(&manifest)?),
    )?;
    println!(
        "{}",
        serde_json::json!({
            "status": "PASS",
            "manifest": output.join("manifest.json"),
            "accountCount": manifest.accounts.len(),
            "minimumContextSlot": minimum_context_slot,
            "commitment": "finalized",
            "endpointRecorded": false,
            "signerLoaded": false,
        })
    );
    Ok(())
}

fn parse_output(args: impl IntoIterator<Item = String>) -> Result<PathBuf, Box<dyn Error>> {
    let mut args = args.into_iter();
    let mut output = None;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--output" => {
                output = Some(PathBuf::from(
                    args.next().ok_or("--output requires a path")?,
                ));
            }
            "--help" | "-h" => return Err(usage().into()),
            _ => return Err(format!("unknown argument {arg}\n{}", usage()).into()),
        }
    }
    output.ok_or_else(|| usage().into())
}

fn usage() -> &'static str {
    "Usage: FLEET_FIXTURE_MAINNET_RPC_URL=https://public-mainnet-rpc fleet-mainnet-clone-capture --output DIR\n\nThe RPC URL must be explicit, unauthenticated HTTPS. The command performs finalized read-only RPC calls, writes no endpoint into the fixture, loads no signer, and sends no transaction."
}

fn validate_public_rpc_url(value: &str) -> Result<(), Box<dyn Error>> {
    let parsed = reqwest::Url::parse(value).map_err(|_| "Mainnet RPC URL is invalid")?;
    if parsed.scheme() != "https"
        || parsed.host_str().is_none()
        || !parsed.username().is_empty()
        || parsed.password().is_some()
        || parsed.query().is_some()
        || parsed.fragment().is_some()
        || parsed.path() != "/"
    {
        return Err("Mainnet fixture capture permits only an unauthenticated HTTPS origin with no path, query, credentials, or fragment".into());
    }
    Ok(())
}

fn fetch_accounts(
    rpc: &RpcClient,
    mut addresses: Vec<Pubkey>,
    minimum_context_slot: u64,
) -> Result<BTreeMap<Pubkey, (Option<Account>, u64)>, Box<dyn Error>> {
    addresses.sort();
    addresses.dedup();
    let mut fetched = BTreeMap::new();
    for chunk in addresses.chunks(GET_MULTIPLE_ACCOUNTS_LIMIT) {
        let response = rpc
            .get_multiple_accounts_with_config(
                chunk,
                RpcAccountInfoConfig {
                    encoding: Some(UiAccountEncoding::Base64),
                    commitment: Some(CommitmentConfig::finalized()),
                    min_context_slot: Some(minimum_context_slot),
                    ..RpcAccountInfoConfig::default()
                },
            )
            .map_err(|_| "finalized account batch acquisition failed")?;
        if response.context.slot < minimum_context_slot || response.value.len() != chunk.len() {
            return Err("finalized account batch violated its snapshot fence".into());
        }
        for (address, account) in chunk.iter().copied().zip(response.value) {
            fetched.insert(address, (account, response.context.slot));
        }
    }
    Ok(fetched)
}

fn upgradeable_program_data_address(account: &Account) -> Result<Pubkey, Box<dyn Error>> {
    if account.data.len() != 36 || account.data[..4] != [2, 0, 0, 0] {
        return Err("upgradeable program account has an unsupported state layout".into());
    }
    Ok(Pubkey::new_from_array(account.data[4..36].try_into()?))
}

fn upgradeable_loader_id() -> Pubkey {
    Pubkey::from_str(UPGRADEABLE_LOADER_ID).expect("canonical upgradeable loader ID")
}

fn sha256(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}
