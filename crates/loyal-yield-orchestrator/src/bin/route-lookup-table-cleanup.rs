use std::{collections::BTreeSet, env, error::Error, str::FromStr};

use base64::{engine::general_purpose::STANDARD as BASE64, Engine as _};
use loyal_yield_orchestrator::{
    keypair_from_string, NeonSqlClient, NeonSqlConfig, YIELD_ROUTER_KEYPAIR_ENV,
};
use serde::Deserialize;
use serde_json::{json, Value};
use solana_client::rpc_client::RpcClient;
use solana_sdk::address_lookup_table::{
    instruction as address_lookup_table_instruction, program as address_lookup_table_program,
    state::{estimate_last_valid_slot, AddressLookupTable},
};
use solana_sdk::{
    commitment_config::CommitmentConfig, pubkey::Pubkey, signature::Signer,
    transaction::Transaction,
};

const DEFAULT_SOLANA_RPC_URL: &str = "https://api.mainnet-beta.solana.com";
const AFFECTED_POLICY_AUTHORITY: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const AUDITED_KEYPAIR_ENVS: &[&str] = &[
    "YIELD_ROUTER_KEYPAIR",
    "POLICY_KEYPAIR",
    "DEPLOYMENT_PK",
    "SOLANA_TESTING_PK",
];

#[derive(Debug)]
struct Options {
    rpc_url: String,
    authorities: Vec<Pubkey>,
    tables: Vec<Pubkey>,
    allowlist: Vec<Pubkey>,
    recipient: Option<Pubkey>,
    execute: bool,
    scan_program_accounts: bool,
    scan_history: bool,
    include_env_authorities: bool,
    limit: usize,
    history_limit: usize,
    min_slot: Option<u64>,
    authority_key_env: Option<String>,
}

#[derive(Debug)]
struct Candidate {
    table_address: Pubkey,
    lamports: u64,
    owner: Pubkey,
    authority: Option<Pubkey>,
    address_count: usize,
    deactivation_slot: u64,
    last_extended_slot: u64,
}

#[derive(Clone, Debug)]
struct HistoryEvent {
    signature: String,
    slot: u64,
    block_time: Option<i64>,
    kind: &'static str,
    table_address: Pubkey,
    authority: Option<Pubkey>,
    payer_or_recipient: Option<Pubkey>,
    new_address_count: Option<usize>,
}

#[derive(Debug, Deserialize)]
struct RpcResponse {
    result: Option<Value>,
    error: Option<RpcError>,
}

#[derive(Debug, Deserialize)]
struct RpcError {
    code: i64,
    message: String,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Response {
    result: Option<ProgramAccountsV2Result>,
    error: Option<ProgramAccountsV2Error>,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Result {
    accounts: Vec<ProgramAccountsV2Account>,
    #[serde(rename = "paginationKey")]
    pagination_key: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Account {
    pubkey: String,
    account: ProgramAccountsV2AccountData,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2AccountData {
    data: Value,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsV2Error {
    code: i64,
    message: String,
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let options = parse_args(env::args().skip(1))?;
    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::confirmed());
    let protected = protected_tables().await?;
    let env_tables = route_lookup_tables_from_env()?;
    let manual_allowlist = options.allowlist.iter().copied().collect::<BTreeSet<_>>();
    let mut protected_all = protected;
    protected_all.extend(env_tables.iter().copied());
    protected_all.extend(manual_allowlist.iter().copied());

    let mut table_addresses = options.tables.clone();
    let history_events = if options.scan_history {
        discover_tables_by_history(&options.rpc_url, &options.authorities, &options).await?
    } else {
        Vec::new()
    };
    table_addresses.extend(history_events.iter().map(|event| event.table_address));
    if options.scan_program_accounts {
        table_addresses.extend(
            discover_tables_by_program_scan(
                &rpc,
                &options.rpc_url,
                &options.authorities,
                options.limit,
            )
            .await?,
        );
    }
    table_addresses.sort();
    table_addresses.dedup();
    if options.limit > 0 && table_addresses.len() > options.limit {
        table_addresses.truncate(options.limit);
    }

    let current_slot = rpc.get_slot()?;
    let signer = if options.execute {
        Some(load_authority_signer(&options)?)
    } else {
        None
    };
    let recipient = options
        .recipient
        .or_else(|| signer.as_ref().map(|signer| signer.pubkey()));
    let mut rows = Vec::new();
    let mut total_reclaimable = 0_u64;
    let mut total_reclaimed = 0_u64;

    for table_address in table_addresses {
        let candidate = match load_candidate(&rpc, table_address) {
            Ok(candidate) => candidate,
            Err(error) => {
                rows.push(json!({
                    "table": table_address.to_string(),
                    "action": "skip",
                    "reason": format!("fetch_or_decode_failed: {error}"),
                }));
                continue;
            }
        };
        let authority_matches = candidate
            .authority
            .is_some_and(|authority| options.authorities.contains(&authority));
        let protected_reason = if protected_all.contains(&candidate.table_address) {
            Some("durable_registry_env_or_allowlist")
        } else {
            None
        };
        let (action, reason) = classify_candidate(
            &candidate,
            authority_matches,
            protected_reason,
            current_slot,
        );
        if matches!(action, "deactivate" | "close") {
            total_reclaimable = total_reclaimable.saturating_add(candidate.lamports);
        }
        let candidate_history = history_events
            .iter()
            .filter(|event| event.table_address == candidate.table_address)
            .map(|event| history_event_json(event, &protected_all))
            .collect::<Vec<_>>();

        let mut execution = Value::Null;
        if options.execute && matches!(action, "deactivate" | "close") {
            let signer = signer
                .as_ref()
                .ok_or("--execute requires an authority signer")?;
            let authority = candidate
                .authority
                .ok_or("candidate had no authority during execute")?;
            if signer.pubkey() != authority {
                return Err(format!(
                    "authority signer {} does not match table {} authority {}",
                    signer.pubkey(),
                    candidate.table_address,
                    authority
                )
                .into());
            }
            if action == "deactivate" {
                let instruction = address_lookup_table_instruction::deactivate_lookup_table(
                    candidate.table_address,
                    signer.pubkey(),
                );
                let signature = send_single_instruction(&rpc, signer.as_ref(), instruction)?;
                execution = json!({
                    "signature": signature,
                    "kind": "deactivate_lookup_table",
                });
                record_deactivated(&candidate.table_address, current_slot, &signature).await?;
            } else {
                let recipient = recipient.ok_or("--recipient is required for close execution")?;
                let instruction = address_lookup_table_instruction::close_lookup_table(
                    candidate.table_address,
                    signer.pubkey(),
                    recipient,
                );
                let signature = send_single_instruction(&rpc, signer.as_ref(), instruction)?;
                total_reclaimed = total_reclaimed.saturating_add(candidate.lamports);
                execution = json!({
                    "signature": signature,
                    "kind": "close_lookup_table",
                    "recipient": recipient.to_string(),
                    "reclaimedLamports": candidate.lamports.to_string(),
                });
                record_closed(
                    &candidate.table_address,
                    &signature,
                    &recipient,
                    candidate.lamports,
                )
                .await?;
            }
        }

        rows.push(json!({
            "table": candidate.table_address.to_string(),
            "owner": candidate.owner.to_string(),
            "authority": candidate.authority.map(|authority| authority.to_string()),
            "status": lookup_table_status(&candidate, current_slot),
            "addressCount": candidate.address_count,
            "lamportsReclaimable": candidate.lamports.to_string(),
            "lastExtendedSlot": candidate.last_extended_slot,
            "deactivationSlot": candidate.deactivation_slot,
            "action": action,
            "reason": reason,
            "historyEvents": candidate_history,
            "execution": execution,
        }));
    }

    println!(
        "{}",
        serde_json::to_string_pretty(&json!({
            "status": if options.execute { "lookup_table_cleanup_execute" } else { "lookup_table_cleanup_dry_run" },
            "execute": options.execute,
            "rpcUrl": redacted_rpc_url(&options.rpc_url),
            "authorities": options.authorities.iter().map(ToString::to_string).collect::<Vec<_>>(),
            "includeEnvAuthorities": options.include_env_authorities,
            "scanProgramAccounts": options.scan_program_accounts,
            "scanHistory": options.scan_history,
            "historyLimit": options.history_limit,
            "minSlot": options.min_slot,
            "explicitTableCount": options.tables.len(),
            "protectedTableCount": protected_all.len(),
            "feesRecoverable": false,
            "feeNote": "ALT account rent can be reclaimed after close; transaction fees are not recoverable.",
            "currentSlot": current_slot,
            "totalReclaimableLamports": total_reclaimable.to_string(),
            "totalReclaimedLamports": total_reclaimed.to_string(),
            "historyEventCount": history_events.len(),
            "historyEvents": history_events.iter().map(|event| history_event_json(event, &protected_all)).collect::<Vec<_>>(),
            "candidates": rows,
        }))?
    );
    Ok(())
}

fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Options, Box<dyn Error>> {
    let mut rpc_url = env::var("SOLANA_RPC_URL").unwrap_or_else(|_| DEFAULT_SOLANA_RPC_URL.into());
    let mut authorities = vec![Pubkey::from_str(AFFECTED_POLICY_AUTHORITY)?];
    let mut tables = Vec::new();
    let mut allowlist = Vec::new();
    let mut recipient = None;
    let mut execute = false;
    let mut scan_program_accounts = false;
    let mut scan_history = false;
    let mut include_env_authorities = false;
    let mut limit = 500_usize;
    let mut history_limit = 100_usize;
    let mut min_slot = None;
    let mut authority_key_env = None;
    let mut iter = args.into_iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--rpc-url" => rpc_url = iter.next().ok_or("--rpc-url requires a value")?,
            "--authority" => authorities.push(parse_pubkey_arg(&arg, iter.next())?),
            "--table" => tables.push(parse_pubkey_arg(&arg, iter.next())?),
            "--allowlist" => allowlist.push(parse_pubkey_arg(&arg, iter.next())?),
            "--recipient" => recipient = Some(parse_pubkey_arg(&arg, iter.next())?),
            "--authority-key-env" => {
                authority_key_env = Some(iter.next().ok_or("--authority-key-env requires a value")?)
            }
            "--execute" => execute = true,
            "--dry-run" => execute = false,
            "--scan-program-accounts" => scan_program_accounts = true,
            "--scan-history" => scan_history = true,
            "--include-env-authorities" => include_env_authorities = true,
            "--limit" => {
                limit = iter
                    .next()
                    .ok_or("--limit requires a value")?
                    .parse()
                    .map_err(|_| "--limit must be a usize")?;
            }
            "--history-limit" => {
                history_limit = iter
                    .next()
                    .ok_or("--history-limit requires a value")?
                    .parse()
                    .map_err(|_| "--history-limit must be a usize")?;
            }
            "--min-slot" => {
                min_slot = Some(
                    iter.next()
                        .ok_or("--min-slot requires a value")?
                        .parse()
                        .map_err(|_| "--min-slot must be a u64")?,
                );
            }
            "--help" | "-h" => {
                println!(
                    "Usage: route-lookup-table-cleanup [--authority <PUBKEY>...] [--include-env-authorities] [--table <PUBKEY>...] [--allowlist <PUBKEY>...] [--recipient <PUBKEY>] [--authority-key-env <ENV>] [--scan-program-accounts] [--scan-history] [--history-limit <N>] [--min-slot <SLOT>] [--execute]\n\nDry-run is the default. Reads SOLANA_RPC_URL, optional NEON_DATABASE_URL, optional YIELD_ROUTE_LOOKUP_TABLES, and by default YIELD_ROUTER_KEYPAIR for execute. --include-env-authorities derives public keys from present YIELD_ROUTER_KEYPAIR, POLICY_KEYPAIR, DEPLOYMENT_PK, and SOLANA_TESTING_PK values without printing secrets."
                );
                std::process::exit(0);
            }
            _ => return Err(format!("unknown argument: {arg}").into()),
        }
    }
    if include_env_authorities {
        authorities.extend(env_authority_pubkeys()?);
    }
    authorities.sort();
    authorities.dedup();
    Ok(Options {
        rpc_url,
        authorities,
        tables,
        allowlist,
        recipient,
        execute,
        scan_program_accounts,
        scan_history,
        include_env_authorities,
        limit,
        history_limit,
        min_slot,
        authority_key_env,
    })
}

fn redacted_rpc_url(rpc_url: &str) -> String {
    match rpc_url.split_once('?') {
        Some((prefix, _)) => format!("{prefix}?<redacted>"),
        None => rpc_url.to_owned(),
    }
}

fn env_authority_pubkeys() -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let mut pubkeys = Vec::new();
    for name in AUDITED_KEYPAIR_ENVS {
        let Ok(value) = env::var(name) else {
            continue;
        };
        pubkeys.push(keypair_from_string(&value)?.pubkey());
    }
    Ok(pubkeys)
}

fn parse_pubkey_arg(flag: &str, value: Option<String>) -> Result<Pubkey, Box<dyn Error>> {
    let raw = value.ok_or_else(|| format!("{flag} requires a public key"))?;
    Pubkey::from_str(&raw).map_err(|_| format!("{flag} value {raw:?} is not a public key").into())
}

async fn protected_tables() -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let Ok(database_url) = env::var("NEON_DATABASE_URL") else {
        return Ok(BTreeSet::new());
    };
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url)).await?;
    let addresses = client.protected_route_lookup_table_addresses().await?;
    parse_pubkey_set(addresses)
}

fn route_lookup_tables_from_env() -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let Ok(raw) = env::var("YIELD_ROUTE_LOOKUP_TABLES") else {
        return Ok(BTreeSet::new());
    };
    parse_pubkey_set(
        raw.split(|c: char| c == ',' || c.is_ascii_whitespace())
            .map(str::to_owned),
    )
}

fn parse_pubkey_set(
    values: impl IntoIterator<Item = String>,
) -> Result<BTreeSet<Pubkey>, Box<dyn Error>> {
    let mut set = BTreeSet::new();
    for value in values {
        let value = value.trim();
        if value.is_empty() {
            continue;
        }
        set.insert(Pubkey::from_str(value)?);
    }
    Ok(set)
}

async fn discover_tables_by_program_scan(
    rpc: &RpcClient,
    rpc_url: &str,
    authorities: &[Pubkey],
    limit: usize,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let accounts = match rpc.get_program_accounts(&address_lookup_table_program::id()) {
        Ok(accounts) => accounts,
        Err(error) => {
            return discover_tables_by_program_scan_v2(rpc_url, authorities, limit)
                .await
                .map_err(|fallback_error| {
                    format!(
                        "getProgramAccounts failed ({error}); getProgramAccountsV2 fallback failed ({fallback_error})"
                    )
                    .into()
                });
        }
    };
    let mut tables = Vec::new();
    for (table_address, account) in accounts {
        let Ok(table) = AddressLookupTable::deserialize(&account.data) else {
            continue;
        };
        if table
            .meta
            .authority
            .is_some_and(|authority| authorities.contains(&authority))
        {
            tables.push(table_address);
            if limit > 0 && tables.len() >= limit {
                break;
            }
        }
    }
    Ok(tables)
}

async fn discover_tables_by_program_scan_v2(
    rpc_url: &str,
    authorities: &[Pubkey],
    limit: usize,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut pagination_key: Option<String> = None;
    let mut tables = Vec::new();

    loop {
        let mut config = json!({
            "encoding": "base64",
            "limit": 1000,
        });
        if let Some(key) = pagination_key.as_ref() {
            config["paginationKey"] = json!(key);
        }
        let response = http
            .post(rpc_url)
            .json(&json!({
                "jsonrpc": "2.0",
                "id": "route-lookup-table-cleanup",
                "method": "getProgramAccountsV2",
                "params": [
                    address_lookup_table_program::id().to_string(),
                    config,
                ],
            }))
            .send()
            .await?
            .error_for_status()?
            .json::<ProgramAccountsV2Response>()
            .await?;
        if let Some(error) = response.error {
            return Err(format!(
                "getProgramAccountsV2 returned {}: {}",
                error.code, error.message
            )
            .into());
        }
        let result = response
            .result
            .ok_or("getProgramAccountsV2 response did not include result")?;
        let account_count = result.accounts.len();
        for account in result.accounts {
            let table_address = Pubkey::from_str(&account.pubkey)?;
            let Some(data) = decode_program_account_data(&account.account.data)? else {
                continue;
            };
            let Ok(table) = AddressLookupTable::deserialize(&data) else {
                continue;
            };
            if table
                .meta
                .authority
                .is_some_and(|authority| authorities.contains(&authority))
            {
                tables.push(table_address);
                if limit > 0 && tables.len() >= limit {
                    return Ok(tables);
                }
            }
        }
        pagination_key = result.pagination_key;
        if pagination_key.is_none() || account_count == 0 {
            break;
        }
    }

    Ok(tables)
}

fn decode_program_account_data(data: &Value) -> Result<Option<Vec<u8>>, Box<dyn Error>> {
    let Some(items) = data.as_array() else {
        return Ok(None);
    };
    let Some(encoded) = items.first().and_then(Value::as_str) else {
        return Ok(None);
    };
    let encoding = items.get(1).and_then(Value::as_str).unwrap_or("base64");
    if encoding != "base64" {
        return Err(format!("unsupported getProgramAccountsV2 account encoding {encoding}").into());
    }
    Ok(Some(BASE64.decode(encoded)?))
}

async fn discover_tables_by_history(
    rpc_url: &str,
    authorities: &[Pubkey],
    options: &Options,
) -> Result<Vec<HistoryEvent>, Box<dyn Error>> {
    let http = reqwest::Client::new();
    let mut events = Vec::new();
    for authority in authorities {
        let signatures = rpc_call(
            &http,
            rpc_url,
            "getSignaturesForAddress",
            json!([
                authority.to_string(),
                {
                    "limit": options.history_limit,
                }
            ]),
        )
        .await?;
        let Some(signatures) = signatures.as_array() else {
            continue;
        };
        for entry in signatures {
            let Some(signature) = entry.get("signature").and_then(Value::as_str) else {
                continue;
            };
            let Some(slot) = entry.get("slot").and_then(Value::as_u64) else {
                continue;
            };
            if options.min_slot.is_some_and(|min_slot| slot < min_slot) {
                continue;
            }
            let block_time = entry.get("blockTime").and_then(Value::as_i64);
            let transaction = rpc_call(
                &http,
                rpc_url,
                "getTransaction",
                json!([
                    signature,
                    {
                        "encoding": "json",
                        "maxSupportedTransactionVersion": 0,
                    }
                ]),
            )
            .await?;
            events.extend(lookup_table_events_from_transaction(
                signature,
                slot,
                block_time,
                &transaction,
                authorities,
            )?);
        }
    }
    events.sort_by(|left, right| {
        right
            .slot
            .cmp(&left.slot)
            .then_with(|| left.signature.cmp(&right.signature))
            .then_with(|| left.kind.cmp(right.kind))
    });
    events.dedup_by(|left, right| {
        left.signature == right.signature
            && left.kind == right.kind
            && left.table_address == right.table_address
    });
    Ok(events)
}

async fn rpc_call(
    http: &reqwest::Client,
    rpc_url: &str,
    method: &str,
    params: Value,
) -> Result<Value, Box<dyn Error>> {
    let response = http
        .post(rpc_url)
        .json(&json!({
            "jsonrpc": "2.0",
            "id": "route-lookup-table-cleanup",
            "method": method,
            "params": params,
        }))
        .send()
        .await?
        .error_for_status()?
        .json::<RpcResponse>()
        .await?;
    if let Some(error) = response.error {
        return Err(format!("RPC {method} returned {}: {}", error.code, error.message).into());
    }
    response
        .result
        .ok_or_else(|| format!("RPC {method} response did not include result").into())
}

fn lookup_table_events_from_transaction(
    signature: &str,
    slot: u64,
    block_time: Option<i64>,
    transaction: &Value,
    audited_authorities: &[Pubkey],
) -> Result<Vec<HistoryEvent>, Box<dyn Error>> {
    let account_keys = transaction
        .pointer("/transaction/message/accountKeys")
        .and_then(Value::as_array)
        .map(|values| {
            values
                .iter()
                .filter_map(account_key_from_value)
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    let mut events = Vec::new();
    if let Some(instructions) = transaction
        .pointer("/transaction/message/instructions")
        .and_then(Value::as_array)
    {
        for instruction in instructions {
            if let Some(event) = lookup_table_event_from_instruction(
                signature,
                slot,
                block_time,
                &account_keys,
                instruction,
                audited_authorities,
            )? {
                events.push(event);
            }
        }
    }
    if let Some(inner_groups) = transaction
        .pointer("/meta/innerInstructions")
        .and_then(Value::as_array)
    {
        for group in inner_groups {
            let Some(instructions) = group.get("instructions").and_then(Value::as_array) else {
                continue;
            };
            for instruction in instructions {
                if let Some(event) = lookup_table_event_from_instruction(
                    signature,
                    slot,
                    block_time,
                    &account_keys,
                    instruction,
                    audited_authorities,
                )? {
                    events.push(event);
                }
            }
        }
    }
    Ok(events)
}

fn account_key_from_value(value: &Value) -> Option<String> {
    value.as_str().map(str::to_owned).or_else(|| {
        value
            .get("pubkey")
            .and_then(Value::as_str)
            .map(str::to_owned)
    })
}

fn lookup_table_event_from_instruction(
    signature: &str,
    slot: u64,
    block_time: Option<i64>,
    account_keys: &[String],
    instruction: &Value,
    audited_authorities: &[Pubkey],
) -> Result<Option<HistoryEvent>, Box<dyn Error>> {
    let Some(program_id) = instruction
        .get("programIdIndex")
        .and_then(Value::as_u64)
        .and_then(|index| account_keys.get(index as usize))
    else {
        return Ok(None);
    };
    if program_id != &address_lookup_table_program::id().to_string() {
        return Ok(None);
    }
    let Some(accounts) = instruction.get("accounts").and_then(Value::as_array) else {
        return Ok(None);
    };
    let instruction_accounts = accounts
        .iter()
        .filter_map(Value::as_u64)
        .filter_map(|index| account_keys.get(index as usize))
        .map(|value| Pubkey::from_str(value))
        .collect::<Result<Vec<_>, _>>()?;
    if !instruction_accounts
        .iter()
        .any(|account| audited_authorities.contains(account))
    {
        return Ok(None);
    }
    let Some(table_address) = instruction_accounts.first().copied() else {
        return Ok(None);
    };
    let authority = instruction_accounts.get(1).copied();
    let payer_or_recipient = instruction_accounts.get(2).copied();
    let Some(data) = instruction.get("data").and_then(Value::as_str) else {
        return Ok(None);
    };
    let decoded = bs58::decode(data).into_vec()?;
    let program_instruction =
        bincode::deserialize::<address_lookup_table_instruction::ProgramInstruction>(&decoded)?;
    let (kind, new_address_count) = match program_instruction {
        address_lookup_table_instruction::ProgramInstruction::CreateLookupTable { .. } => {
            ("create", None)
        }
        address_lookup_table_instruction::ProgramInstruction::ExtendLookupTable {
            new_addresses,
        } => ("extend", Some(new_addresses.len())),
        address_lookup_table_instruction::ProgramInstruction::DeactivateLookupTable => {
            ("deactivate", None)
        }
        address_lookup_table_instruction::ProgramInstruction::CloseLookupTable => ("close", None),
        address_lookup_table_instruction::ProgramInstruction::FreezeLookupTable => ("freeze", None),
    };
    Ok(Some(HistoryEvent {
        signature: signature.to_owned(),
        slot,
        block_time,
        kind,
        table_address,
        authority,
        payer_or_recipient,
        new_address_count,
    }))
}

fn history_event_json(event: &HistoryEvent, protected_tables: &BTreeSet<Pubkey>) -> Value {
    json!({
        "signature": event.signature,
        "slot": event.slot,
        "blockTime": event.block_time,
        "kind": event.kind,
        "classification": history_event_classification(event, protected_tables),
        "table": event.table_address.to_string(),
        "authority": event.authority.map(|authority| authority.to_string()),
        "payerOrRecipient": event.payer_or_recipient.map(|value| value.to_string()),
        "newAddressCount": event.new_address_count,
    })
}

fn history_event_classification(
    event: &HistoryEvent,
    protected_tables: &BTreeSet<Pubkey>,
) -> &'static str {
    match event.kind {
        "create" | "extend" if protected_tables.contains(&event.table_address) => {
            "expected_provisioning_protected"
        }
        "create" | "extend" => "unexpected_create_extend",
        "deactivate" => "cleanup_deactivate",
        "close" => "cleanup_close",
        _ => "other_lookup_table_instruction",
    }
}

fn load_candidate(rpc: &RpcClient, table_address: Pubkey) -> Result<Candidate, Box<dyn Error>> {
    let account = rpc.get_account(&table_address)?;
    let table = AddressLookupTable::deserialize(&account.data).map_err(|error| {
        format!("failed to deserialize address lookup table {table_address}: {error:?}")
    })?;
    Ok(Candidate {
        table_address,
        lamports: account.lamports,
        owner: account.owner,
        authority: table.meta.authority,
        address_count: table.addresses.len(),
        deactivation_slot: table.meta.deactivation_slot,
        last_extended_slot: table.meta.last_extended_slot,
    })
}

fn classify_candidate(
    candidate: &Candidate,
    authority_matches: bool,
    protected_reason: Option<&str>,
    current_slot: u64,
) -> (&'static str, String) {
    if candidate.owner != address_lookup_table_program::id() {
        return (
            "skip",
            "account_not_owned_by_address_lookup_table_program".to_owned(),
        );
    }
    if let Some(reason) = protected_reason {
        return ("skip", reason.to_owned());
    }
    if candidate.authority.is_none() {
        return ("skip", "lookup_table_has_no_close_authority".to_owned());
    }
    if !authority_matches {
        return ("skip", "authority_not_in_audited_set".to_owned());
    }
    if candidate.deactivation_slot == u64::MAX {
        return ("deactivate", "active_orphan_table".to_owned());
    }
    if current_slot <= estimate_last_valid_slot(candidate.deactivation_slot) {
        return (
            "defer",
            format!(
                "deactivation_slot_recent_until_at_least_{}",
                estimate_last_valid_slot(candidate.deactivation_slot)
            ),
        );
    }
    (
        "close",
        "deactivated_orphan_table_cooldown_elapsed".to_owned(),
    )
}

fn lookup_table_status(candidate: &Candidate, current_slot: u64) -> &'static str {
    if candidate.deactivation_slot == u64::MAX {
        "active"
    } else if current_slot <= estimate_last_valid_slot(candidate.deactivation_slot) {
        "deactivating"
    } else {
        "deactivated"
    }
}

fn load_authority_signer(options: &Options) -> Result<Box<dyn Signer>, Box<dyn Error>> {
    let env_name = options
        .authority_key_env
        .as_deref()
        .unwrap_or(YIELD_ROUTER_KEYPAIR_ENV);
    let value = env::var(env_name).map_err(|_| format!("{env_name} must be set for --execute"))?;
    Ok(Box::new(keypair_from_string(&value)?))
}

fn send_single_instruction(
    rpc: &RpcClient,
    signer: &dyn Signer,
    instruction: solana_sdk::instruction::Instruction,
) -> Result<String, Box<dyn Error>> {
    let blockhash = rpc.get_latest_blockhash()?;
    let transaction = Transaction::new_signed_with_payer(
        &[instruction],
        Some(&signer.pubkey()),
        &[signer],
        blockhash,
    );
    Ok(rpc.send_and_confirm_transaction(&transaction)?.to_string())
}

async fn record_deactivated(
    table_address: &Pubkey,
    slot: u64,
    signature: &str,
) -> Result<(), Box<dyn Error>> {
    let Ok(database_url) = env::var("NEON_DATABASE_URL") else {
        return Ok(());
    };
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url)).await?;
    let _ = client
        .mark_route_lookup_table_deactivated(
            &table_address.to_string(),
            i64::try_from(slot)?,
            signature,
        )
        .await;
    Ok(())
}

async fn record_closed(
    table_address: &Pubkey,
    signature: &str,
    recipient: &Pubkey,
    reclaimed_lamports: u64,
) -> Result<(), Box<dyn Error>> {
    let Ok(database_url) = env::var("NEON_DATABASE_URL") else {
        return Ok(());
    };
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url)).await?;
    let _ = client
        .mark_route_lookup_table_closed(
            &table_address.to_string(),
            signature,
            &recipient.to_string(),
            i64::try_from(reclaimed_lamports)?,
        )
        .await;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn candidate(authority: Pubkey, deactivation_slot: u64) -> Candidate {
        Candidate {
            table_address: Pubkey::new_unique(),
            lamports: 1_234_567,
            owner: address_lookup_table_program::id(),
            authority: Some(authority),
            address_count: 3,
            deactivation_slot,
            last_extended_slot: 0,
        }
    }

    #[test]
    fn alt_cleanup_classifies_active_audited_table_for_deactivation() {
        let authority = Pubkey::new_unique();
        let candidate = candidate(authority, u64::MAX);

        let (action, reason) = classify_candidate(&candidate, true, None, 100);

        assert_eq!(action, "deactivate");
        assert_eq!(reason, "active_orphan_table");
    }

    #[test]
    fn alt_cleanup_skips_protected_table() {
        let authority = Pubkey::new_unique();
        let candidate = candidate(authority, u64::MAX);

        let (action, reason) = classify_candidate(
            &candidate,
            true,
            Some("durable_registry_env_or_allowlist"),
            100,
        );

        assert_eq!(action, "skip");
        assert_eq!(reason, "durable_registry_env_or_allowlist");
    }

    #[test]
    fn alt_cleanup_classifies_elapsed_deactivated_table_for_close() {
        let authority = Pubkey::new_unique();
        let deactivation_slot = 10;
        let current_slot = estimate_last_valid_slot(deactivation_slot) + 1;
        let candidate = candidate(authority, deactivation_slot);

        let (action, reason) = classify_candidate(&candidate, true, None, current_slot);

        assert_eq!(action, "close");
        assert_eq!(reason, "deactivated_orphan_table_cooldown_elapsed");
        assert_eq!(lookup_table_status(&candidate, current_slot), "deactivated");
    }

    #[test]
    fn alt_cleanup_defers_recently_deactivated_table() {
        let authority = Pubkey::new_unique();
        let deactivation_slot = 10;
        let current_slot = estimate_last_valid_slot(deactivation_slot);
        let candidate = candidate(authority, deactivation_slot);

        let (action, reason) = classify_candidate(&candidate, true, None, current_slot);

        assert_eq!(action, "defer");
        assert!(reason.starts_with("deactivation_slot_recent_until_at_least_"));
        assert_eq!(
            lookup_table_status(&candidate, current_slot),
            "deactivating"
        );
    }

    #[test]
    fn alt_cleanup_skips_authority_mismatch() {
        let authority = Pubkey::new_unique();
        let candidate = candidate(authority, u64::MAX);

        let (action, reason) = classify_candidate(&candidate, false, None, 100);

        assert_eq!(action, "skip");
        assert_eq!(reason, "authority_not_in_audited_set");
    }

    #[test]
    fn alt_cleanup_redacts_rpc_query_string() {
        assert_eq!(
            redacted_rpc_url("https://mainnet.helius-rpc.com/?api-key=secret"),
            "https://mainnet.helius-rpc.com/?<redacted>"
        );
        assert_eq!(
            redacted_rpc_url("http://localhost:8899"),
            "http://localhost:8899"
        );
    }

    #[test]
    fn alt_cleanup_decodes_create_history_instruction() {
        let authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let (instruction, lookup_table) =
            address_lookup_table_instruction::create_lookup_table(authority, payer, 123);
        let account_keys = account_keys_for_instruction(&instruction);
        let instruction_json = compiled_instruction_json(&instruction, &account_keys);

        let event = lookup_table_event_from_instruction(
            "signature",
            456,
            Some(789),
            &account_keys,
            &instruction_json,
            &[authority],
        )
        .expect("history parser should decode create")
        .expect("create instruction should produce an event");

        assert_eq!(event.kind, "create");
        assert_eq!(event.table_address, lookup_table);
        assert_eq!(event.authority, Some(authority));
        assert_eq!(event.payer_or_recipient, Some(payer));
        assert_eq!(event.slot, 456);
        assert_eq!(event.block_time, Some(789));
    }

    #[test]
    fn alt_cleanup_decodes_extend_history_instruction() {
        let lookup_table = Pubkey::new_unique();
        let authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let new_addresses = vec![Pubkey::new_unique(), Pubkey::new_unique()];
        let instruction = address_lookup_table_instruction::extend_lookup_table(
            lookup_table,
            authority,
            Some(payer),
            new_addresses,
        );
        let account_keys = account_keys_for_instruction(&instruction);
        let instruction_json = compiled_instruction_json(&instruction, &account_keys);

        let event = lookup_table_event_from_instruction(
            "signature",
            456,
            None,
            &account_keys,
            &instruction_json,
            &[authority],
        )
        .expect("history parser should decode extend")
        .expect("extend instruction should produce an event");

        assert_eq!(event.kind, "extend");
        assert_eq!(event.table_address, lookup_table);
        assert_eq!(event.authority, Some(authority));
        assert_eq!(event.payer_or_recipient, Some(payer));
        assert_eq!(event.new_address_count, Some(2));
    }

    #[test]
    fn alt_cleanup_history_ignores_unaudited_instruction() {
        let audited_authority = Pubkey::new_unique();
        let other_authority = Pubkey::new_unique();
        let payer = Pubkey::new_unique();
        let (instruction, _) =
            address_lookup_table_instruction::create_lookup_table(other_authority, payer, 123);
        let account_keys = account_keys_for_instruction(&instruction);
        let instruction_json = compiled_instruction_json(&instruction, &account_keys);

        let event = lookup_table_event_from_instruction(
            "signature",
            456,
            None,
            &account_keys,
            &instruction_json,
            &[audited_authority],
        )
        .expect("history parser should decode instruction data");

        assert!(event.is_none());
    }

    #[test]
    fn alt_cleanup_classifies_history_create_extend_events() {
        let table_address = Pubkey::new_unique();
        let event = HistoryEvent {
            signature: "signature".to_owned(),
            slot: 1,
            block_time: None,
            kind: "create",
            table_address,
            authority: None,
            payer_or_recipient: None,
            new_address_count: None,
        };
        let mut protected = BTreeSet::new();

        assert_eq!(
            history_event_classification(&event, &protected),
            "unexpected_create_extend"
        );

        protected.insert(table_address);
        assert_eq!(
            history_event_classification(&event, &protected),
            "expected_provisioning_protected"
        );
    }

    fn account_keys_for_instruction(
        instruction: &solana_sdk::instruction::Instruction,
    ) -> Vec<String> {
        let mut keys = instruction
            .accounts
            .iter()
            .map(|meta| meta.pubkey.to_string())
            .collect::<Vec<_>>();
        keys.push(instruction.program_id.to_string());
        keys.sort();
        keys.dedup();
        keys
    }

    fn compiled_instruction_json(
        instruction: &solana_sdk::instruction::Instruction,
        account_keys: &[String],
    ) -> Value {
        let program_id_index = account_keys
            .iter()
            .position(|key| key == &instruction.program_id.to_string())
            .expect("program id should be present");
        let accounts = instruction
            .accounts
            .iter()
            .map(|meta| {
                account_keys
                    .iter()
                    .position(|key| key == &meta.pubkey.to_string())
                    .expect("instruction account should be present")
            })
            .collect::<Vec<_>>();
        json!({
            "programIdIndex": program_id_index,
            "accounts": accounts,
            "data": bs58::encode(&instruction.data).into_string(),
        })
    }
}
