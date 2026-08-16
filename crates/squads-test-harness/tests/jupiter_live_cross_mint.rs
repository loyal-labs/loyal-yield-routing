use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use loyal_actions::{
    compile_squads_inner_instruction, derive_classic_associated_token_account,
    execute_program_interaction_policy_instruction,
    jupiter::{
        parse_and_validate_jupiter_exact_in_build, JupiterBuildLimits,
        JupiterExactInBuildExpectation, JupiterLookupTableSnapshot, JupiterMintSnapshot,
        JupiterTokenAccountSnapshot, ALPHAQ_PROGRAM_ID, JUPITER_EVENT_AUTHORITY,
        SOLANA_MAX_COMPUTE_UNITS, SOLANA_PACKET_DATA_SIZE,
    },
    JUPITER_V6_PROGRAM_ID, USDC_MINT, USDT_MINT,
};
use serde::Deserialize;
use solana_client::{rpc_client::RpcClient, rpc_config::RpcSimulateTransactionConfig};
#[allow(deprecated)]
use solana_sdk::address_lookup_table::{
    program as address_lookup_table_program, state::AddressLookupTable,
};
use solana_sdk::{
    account::Account,
    commitment_config::CommitmentConfig,
    compute_budget::ComputeBudgetInstruction,
    instruction::AccountMeta,
    message::{v0, AddressLookupTableAccount, VersionedMessage},
    program_pack::Pack,
    pubkey::Pubkey,
    signature::{Keypair, Signature, Signer},
    transaction::VersionedTransaction,
};
use spl_token::state::Account as SplTokenAccount;
use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    error::Error,
    str::FromStr,
    time::Duration,
};

const LIVE_GATE: &str = "JUPITER_LIVE_CROSS_MINT";
const LIVE_TAKER: &str = "JUPITER_LIVE_TAKER";
const LIVE_FEE_PAYER: &str = "JUPITER_LIVE_FEE_PAYER";
const DEFAULT_BUILD_URL: &str = "https://api.jup.ag/swap/v2/build";
const INPUT_AMOUNT_RAW: u64 = 1_000_000;
const MAXIMUM_SLIPPAGE_BPS: u16 = 50;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LiveBuildEnvelope {
    input_mint: String,
    output_mint: String,
    in_amount: String,
    other_amount_threshold: String,
    slippage_bps: u16,
    addresses_by_lookup_table_address: BTreeMap<String, Vec<String>>,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountsRpcResponse {
    result: Option<ProgramAccountsPage>,
    error: Option<serde_json::Value>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProgramAccountsPage {
    accounts: Vec<ProgramAccountEntry>,
    pagination_key: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountEntry {
    pubkey: String,
    account: ProgramAccountValue,
}

#[derive(Debug, Deserialize)]
struct ProgramAccountValue {
    data: Vec<String>,
}

struct FundedClassicAtaPair {
    authority: Pubkey,
    input_account: Account,
    output_account: Account,
}

#[test]
#[ignore = "requires JUPITER_LIVE_CROSS_MINT=1 and finalized mainnet RPC"]
fn current_classic_spl_jupiter_build_compiles_fits_and_simulates_without_send() {
    if env::var(LIVE_GATE).ok().as_deref() != Some("1") {
        panic!("{LIVE_GATE}=1 is required; run the explicit live verifier wrapper");
    }

    run_live_verifier().expect("current Jupiter classic-SPL build verifier");
}

fn run_live_verifier() -> Result<(), Box<dyn Error>> {
    let rpc_url = env::var("SOLANA_RPC_URL")?;
    let rpc = RpcClient::new_with_commitment(rpc_url.clone(), CommitmentConfig::finalized());
    let FundedClassicAtaPair {
        authority,
        input_account: input_token_account,
        output_account: output_token_account,
    } = funded_classic_ata_pair(&rpc, &rpc_url, USDC_MINT, USDT_MINT, INPUT_AMOUNT_RAW)?;
    let fee_payer = env::var(LIVE_FEE_PAYER)
        .ok()
        .map(|value| Pubkey::from_str(&value))
        .transpose()?
        .unwrap_or(authority);
    if rpc.get_balance(&fee_payer)? < 10_000 {
        return Err(format!(
            "live simulation fee payer has insufficient lamports; set {LIVE_FEE_PAYER} to a funded public key"
        )
        .into());
    }

    let build_bytes = fetch_current_build(authority)?;
    let envelope: LiveBuildEnvelope = serde_json::from_slice(&build_bytes)?;
    if envelope.input_mint != USDC_MINT.to_string()
        || envelope.output_mint != USDT_MINT.to_string()
        || envelope.in_amount != INPUT_AMOUNT_RAW.to_string()
        || envelope.slippage_bps > MAXIMUM_SLIPPAGE_BPS
    {
        return Err(
            "Jupiter build identity differs from the requested USDC to USDT ExactIn lane".into(),
        );
    }

    let input_mint_account = finalized_account(&rpc, USDC_MINT)?;
    let output_mint_account = finalized_account(&rpc, USDT_MINT)?;
    let lookup_tables = finalized_lookup_tables(&rpc, &envelope.addresses_by_lookup_table_address)?;
    let expected = JupiterExactInBuildExpectation {
        authority,
        input_mint: JupiterMintSnapshot {
            address: USDC_MINT,
            owner_program: input_mint_account.owner,
            data: input_mint_account.data,
        },
        output_mint: JupiterMintSnapshot {
            address: USDT_MINT,
            owner_program: output_mint_account.owner,
            data: output_mint_account.data,
        },
        input_token_account: JupiterTokenAccountSnapshot {
            address: derive_classic_associated_token_account(authority, USDC_MINT),
            owner_program: input_token_account.owner,
            data: input_token_account.data,
        },
        output_token_account: JupiterTokenAccountSnapshot {
            address: derive_classic_associated_token_account(authority, USDT_MINT),
            owner_program: output_token_account.owner,
            data: output_token_account.data,
        },
        additional_token_accounts: vec![],
        input_amount: INPUT_AMOUNT_RAW,
        minimum_output_amount: envelope.other_amount_threshold.parse()?,
        maximum_slippage_bps: MAXIMUM_SLIPPAGE_BPS,
        requested_platform_fee_bps: 0,
        lookup_tables: lookup_tables
            .iter()
            .map(|table| JupiterLookupTableSnapshot {
                address: table.key,
                addresses: table.addresses.clone(),
            })
            .collect(),
        limits: JupiterBuildLimits::default(),
    };
    let validated =
        parse_and_validate_jupiter_exact_in_build(&build_bytes, &expected).map_err(|error| {
            let lookup_address_count = envelope
                .addresses_by_lookup_table_address
                .values()
                .map(Vec::len)
                .sum::<usize>();
            let maximum_table_addresses = envelope
                .addresses_by_lookup_table_address
                .values()
                .map(Vec::len)
                .max()
                .unwrap_or_default();
            format!(
                "{error}; lookupTables={} lookupAddresses={lookup_address_count} maximumTableAddresses={maximum_table_addresses}; extraAccountPrivileges={:?}; live string-shape mismatches: {:?}",
                envelope.addresses_by_lookup_table_address.len(),
                jupiter_extra_account_privileges(&build_bytes, authority),
                jupiter_string_shape_mismatches(&build_bytes)
            )
        })?;
    let live_account_state = validate_live_alphaq_account_states(&rpc, &build_bytes, authority)?;
    let redundant_setup_instruction_count = validated.setup_instructions.len();
    let (route_blockhash, route_last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    if rpc.get_block_height()? > route_last_valid_block_height {
        return Err("same-RPC route blockhash expired before compile and simulation".into());
    }

    let raw_instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(
            validated.compute_budget.simulation_unit_limit,
        ),
        ComputeBudgetInstruction::set_compute_unit_price(
            validated.compute_budget.unit_price_micro_lamports,
        ),
        validated.swap_instruction.clone(),
    ];
    let raw_message = v0::Message::try_compile(
        &fee_payer,
        &raw_instructions,
        &validated.lookup_tables,
        route_blockhash,
    )?;
    let raw_signature_count = usize::from(raw_message.header.num_required_signatures);
    let raw_transaction = VersionedTransaction {
        signatures: vec![Signature::default(); raw_signature_count],
        message: VersionedMessage::V0(raw_message),
    };
    let raw_packet_bytes = bincode::serialize(&raw_transaction)?.len();
    if raw_packet_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err(format!(
            "current raw Jupiter transaction is {raw_packet_bytes} bytes, above the packet limit"
        )
        .into());
    }

    let simulation = rpc.simulate_transaction_with_config(
        &raw_transaction,
        RpcSimulateTransactionConfig {
            sig_verify: false,
            replace_recent_blockhash: false,
            commitment: Some(CommitmentConfig::finalized()),
            ..RpcSimulateTransactionConfig::default()
        },
    )?;
    if let Some(error) = simulation.value.err {
        return Err(format!("current raw Jupiter transaction simulation failed: {error:?}").into());
    }

    let delegated_signer = Keypair::new();
    let action_account = Pubkey::new_unique();
    let mut transaction_accounts = Vec::<AccountMeta>::new();
    let compiled_swap =
        compile_squads_inner_instruction(&mut transaction_accounts, validated.swap_instruction);
    let wrapped_swap = execute_program_interaction_policy_instruction(
        action_account,
        delegated_signer.pubkey(),
        0,
        vec![compiled_swap],
        vec![0],
        transaction_accounts,
    );
    let wrapped_instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
        ComputeBudgetInstruction::set_compute_unit_price(
            validated.compute_budget.unit_price_micro_lamports,
        ),
        wrapped_swap,
    ];
    let wrapped_message = v0::Message::try_compile(
        &delegated_signer.pubkey(),
        &wrapped_instructions,
        &validated.lookup_tables,
        route_blockhash,
    )?;
    let wrapped_transaction =
        VersionedTransaction::try_new(VersionedMessage::V0(wrapped_message), &[&delegated_signer])?;
    let wrapped_packet_bytes = bincode::serialize(&wrapped_transaction)?.len();
    if wrapped_packet_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err(format!(
            "current policy-wrapped Jupiter transaction is {wrapped_packet_bytes} bytes, above the packet limit"
        )
        .into());
    }

    eprintln!(
        "live Jupiter cross-mint verifier PASS: rawPacketBytes={raw_packet_bytes} wrappedPacketBytes={wrapped_packet_bytes} unitsConsumed={} lookupTables={} redundantSetupInstructionsOmitted={redundant_setup_instruction_count} {live_account_state} productionTransactionSent=false",
        simulation.value.units_consumed.unwrap_or_default(),
        validated.lookup_tables.len(),
    );
    Ok(())
}

fn jupiter_extra_account_privileges(json: &[u8], authority: Pubkey) -> Vec<String> {
    let Ok(value) = serde_json::from_slice::<serde_json::Value>(json) else {
        return vec!["response:not_json".to_owned()];
    };
    let authority = authority.to_string();
    value
        .pointer("/swapInstruction/accounts")
        .and_then(serde_json::Value::as_array)
        .into_iter()
        .flatten()
        .enumerate()
        .skip(6)
        .filter_map(|(index, account)| {
            let signer = account.get("isSigner").and_then(serde_json::Value::as_bool)?;
            let writable = account
                .get("isWritable")
                .and_then(serde_json::Value::as_bool)?;
            let authority_repeated =
                account.get("pubkey").and_then(serde_json::Value::as_str) == Some(authority.as_str());
            (signer || authority_repeated).then(|| {
                format!(
                    "index={index},signer={signer},writable={writable},authorityRepeated={authority_repeated}"
                )
            })
        })
        .collect()
}

fn jupiter_string_shape_mismatches(json: &[u8]) -> Vec<String> {
    let Ok(value) = serde_json::from_slice::<serde_json::Value>(json) else {
        return vec!["response:not_json".to_owned()];
    };
    let mut mismatches = Vec::new();
    if let Some(routes) = value.get("routePlan").and_then(serde_json::Value::as_array) {
        for (index, route) in routes.iter().enumerate() {
            let Some(swap) = route.get("swapInfo") else {
                continue;
            };
            for field in [
                "ammKey",
                "label",
                "inputMint",
                "outputMint",
                "inAmount",
                "outAmount",
            ] {
                record_non_string(
                    &mut mismatches,
                    &format!("routePlan[{index}].swapInfo.{field}"),
                    swap.get(field),
                );
            }
        }
    }
    for field in [
        "computeBudgetInstructions",
        "setupInstructions",
        "otherInstructions",
    ] {
        if let Some(instructions) = value.get(field).and_then(serde_json::Value::as_array) {
            for (index, instruction) in instructions.iter().enumerate() {
                record_instruction_shape(
                    &mut mismatches,
                    &format!("{field}[{index}]"),
                    instruction,
                );
            }
        }
    }
    for field in ["swapInstruction", "cleanupInstruction", "tipInstruction"] {
        if let Some(instruction) = value.get(field).filter(|value| !value.is_null()) {
            record_instruction_shape(&mut mismatches, field, instruction);
        }
    }
    record_non_string(
        &mut mismatches,
        "blockhashWithMetadata.fetchedAt",
        value.pointer("/blockhashWithMetadata/fetchedAt"),
    );
    mismatches
}

fn record_instruction_shape(
    mismatches: &mut Vec<String>,
    path: &str,
    instruction: &serde_json::Value,
) {
    for field in ["programId", "data"] {
        record_non_string(
            mismatches,
            &format!("{path}.{field}"),
            instruction.get(field),
        );
    }
    if let Some(accounts) = instruction
        .get("accounts")
        .and_then(serde_json::Value::as_array)
    {
        for (index, account) in accounts.iter().enumerate() {
            record_non_string(
                mismatches,
                &format!("{path}.accounts[{index}].pubkey"),
                account.get("pubkey"),
            );
        }
    }
}

fn record_non_string(mismatches: &mut Vec<String>, path: &str, value: Option<&serde_json::Value>) {
    let Some(value) = value else {
        mismatches.push(format!("{path}:missing"));
        return;
    };
    if !value.is_string() {
        let kind = match value {
            serde_json::Value::Null => "null",
            serde_json::Value::Bool(_) => "bool",
            serde_json::Value::Number(_) => "number",
            serde_json::Value::String(_) => "string",
            serde_json::Value::Array(_) => "array",
            serde_json::Value::Object(_) => "object",
        };
        let detail = if matches!(value, serde_json::Value::Object(_)) {
            format!("={value}")
        } else {
            String::new()
        };
        mismatches.push(format!("{path}:{kind}{detail}"));
    }
}

fn fetch_current_build(authority: Pubkey) -> Result<Vec<u8>, Box<dyn Error>> {
    let build_url =
        env::var("JUPITER_SWAP_BUILD_URL").unwrap_or_else(|_| DEFAULT_BUILD_URL.to_owned());
    let url = format!(
        "{build_url}?inputMint={USDC_MINT}&outputMint={USDT_MINT}&amount={INPUT_AMOUNT_RAW}&taker={authority}&maxAccounts=48&slippageBps={MAXIMUM_SLIPPAGE_BPS}&onlyDirectRoutes=true&dexes=AlphaQ"
    );
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()?;
    let mut request = client.get(url);
    if let Ok(api_key) = env::var("JUPITER_API_KEY") {
        request = request.header("x-api-key", api_key);
    }
    Ok(request.send()?.error_for_status()?.bytes()?.to_vec())
}

fn validate_live_alphaq_account_states(
    rpc: &RpcClient,
    json: &[u8],
    authority: Pubkey,
) -> Result<String, Box<dyn Error>> {
    let value: serde_json::Value = serde_json::from_slice(json)?;
    let route = value
        .pointer("/routePlan/0/swapInfo")
        .ok_or("live build omitted routePlan[0].swapInfo")?;
    let accounts = value
        .pointer("/swapInstruction/accounts")
        .and_then(serde_json::Value::as_array)
        .ok_or("live build omitted swapInstruction.accounts")?;
    let addresses = accounts
        .iter()
        .map(|account| {
            account
                .get("pubkey")
                .and_then(serde_json::Value::as_str)
                .ok_or("live swap account omitted pubkey")
                .and_then(|address| Pubkey::from_str(address).map_err(|_| "invalid live pubkey"))
        })
        .collect::<Result<Vec<_>, _>>()?;
    let states = rpc
        .get_multiple_accounts_with_commitment(&addresses, CommitmentConfig::finalized())?
        .value;
    if states.len() != 24 {
        return Err("accepted AlphaQ instruction did not resolve exactly 24 account states".into());
    }
    for (index, state) in states.iter().enumerate() {
        let expected_executable = matches!(index, 5 | 6 | 7 | 9 | 10 | 21 | 23);
        if let Some(state) = state {
            if state.executable != expected_executable {
                return Err(format!(
                    "accepted AlphaQ account {index} has unexpected executable state"
                )
                .into());
            }
        } else if index != 22 {
            return Err(format!("accepted AlphaQ account {index} is missing at finalized").into());
        }
    }
    for (index, expected_owner) in [
        (0, solana_sdk::system_program::ID),
        (1, spl_token::id()),
        (2, spl_token::id()),
        (3, spl_token::id()),
        (4, spl_token::id()),
        (8, solana_sdk::system_program::ID),
        (11, solana_sdk::system_program::ID),
        (12, ALPHAQ_PROGRAM_ID),
        (13, ALPHAQ_PROGRAM_ID),
        (14, spl_token::id()),
        (15, spl_token::id()),
        (16, spl_token::id()),
        (17, spl_token::id()),
        (18, spl_token::id()),
        (19, spl_token::id()),
        (20, spl_token::id()),
    ] {
        if states[index].as_ref().map(|account| account.owner) != Some(expected_owner) {
            return Err(format!("accepted AlphaQ account {index} has unexpected owner").into());
        }
    }
    for (index, program) in [
        (5, spl_token::id()),
        (6, spl_token::id()),
        (7, JUPITER_V6_PROGRAM_ID),
        (9, JUPITER_V6_PROGRAM_ID),
        (10, ALPHAQ_PROGRAM_ID),
        (21, spl_token::id()),
        (23, JUPITER_V6_PROGRAM_ID),
    ] {
        if addresses[index] != program {
            return Err(format!("accepted AlphaQ account {index} is an unexpected program").into());
        }
    }
    if addresses[8] != JUPITER_EVENT_AUTHORITY
        || addresses[12].to_string() != route["ammKey"].as_str().unwrap_or_default()
    {
        return Err("accepted AlphaQ route metadata does not match its fixed accounts".into());
    }
    let pool_mints = [16usize, 17]
        .map(|index| {
            let token = SplTokenAccount::unpack(
                &states[index]
                    .as_ref()
                    .expect("checked finalized pool token account")
                    .data,
            )
            .map_err(|_| "accepted AlphaQ pool token account is invalid")?;
            if token.owner == authority {
                return Err("AlphaQ pool token account is vault-owned");
            }
            Ok(token.mint)
        })
        .into_iter()
        .collect::<Result<BTreeSet<_>, &str>>()?;
    if pool_mints != BTreeSet::from([USDC_MINT, USDT_MINT]) {
        return Err("AlphaQ pool token accounts do not bind the requested classic pair".into());
    }
    Ok(format!(
        "routeLabel={} accountCount=24 residualExecutables=allowlisted poolTokenAuthorityNotVault=true",
        route["label"].as_str().unwrap_or("invalid"),
    ))
}

fn funded_classic_ata_pair(
    rpc: &RpcClient,
    rpc_url: &str,
    input_mint: Pubkey,
    output_mint: Pubkey,
    input_amount: u64,
) -> Result<FundedClassicAtaPair, Box<dyn Error>> {
    if let Ok(authorities) = env::var(LIVE_TAKER) {
        for authority in authorities.split(',').filter(|value| !value.is_empty()) {
            if let Some(pair) = checked_classic_ata_pair(
                rpc,
                Pubkey::from_str(authority)?,
                input_mint,
                output_mint,
                input_amount,
            )? {
                return Ok(pair);
            }
        }
        return Err(format!(
            "none of the bounded {LIVE_TAKER} candidates owns funded canonical classic-SPL input and output ATAs"
        )
        .into());
    }

    discover_classic_ata_pair(rpc, rpc_url, input_mint, output_mint, input_amount)
}

fn discover_classic_ata_pair(
    rpc: &RpcClient,
    rpc_url: &str,
    input_mint: Pubkey,
    output_mint: Pubkey,
    input_amount: u64,
) -> Result<FundedClassicAtaPair, Box<dyn Error>> {
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()?;
    let mut pagination_key: Option<String> = None;
    for _ in 0..5 {
        let mut config = serde_json::json!({
            "encoding": "base64",
            "commitment": "finalized",
            "filters": [
                {"dataSize": SplTokenAccount::LEN},
                {"memcmp": {"offset": 0, "bytes": output_mint.to_string()}}
            ],
            "limit": 1000
        });
        if let Some(key) = pagination_key.as_ref() {
            config["paginationKey"] = serde_json::Value::String(key.clone());
        }
        let response: ProgramAccountsRpcResponse = client
            .post(rpc_url)
            .json(&serde_json::json!({
                "jsonrpc": "2.0",
                "id": "cross-mint-live-verifier",
                "method": "getProgramAccountsV2",
                "params": [spl_token::id().to_string(), config]
            }))
            .send()?
            .error_for_status()?
            .json()?;
        let page = response.result.ok_or_else(|| {
            format!(
                "getProgramAccountsV2 failed while discovering a setup-free taker: {}",
                response.error.unwrap_or(serde_json::Value::Null)
            )
        })?;

        let mut seen = BTreeSet::new();
        let mut candidates = Vec::new();
        for entry in page.accounts {
            let Some(encoded) = entry.account.data.first() else {
                continue;
            };
            let Ok(data) = BASE64_STANDARD.decode(encoded) else {
                continue;
            };
            let Ok(output_token) = SplTokenAccount::unpack(&data) else {
                continue;
            };
            let Ok(output_address) = Pubkey::from_str(&entry.pubkey) else {
                continue;
            };
            let authority = output_token.owner;
            if output_token.mint != output_mint
                || output_address != derive_classic_associated_token_account(authority, output_mint)
                || !seen.insert(authority)
            {
                continue;
            }
            candidates.push((
                authority,
                derive_classic_associated_token_account(authority, input_mint),
            ));
        }

        for chunk in candidates.chunks(100) {
            let input_addresses = chunk
                .iter()
                .map(|(_, address)| *address)
                .collect::<Vec<_>>();
            let input_accounts = rpc
                .get_multiple_accounts_with_commitment(
                    &input_addresses,
                    CommitmentConfig::finalized(),
                )?
                .value;
            for ((authority, _), input_account) in chunk.iter().zip(input_accounts) {
                let Some(input_account) = input_account else {
                    continue;
                };
                if input_account.owner != spl_token::id() {
                    continue;
                }
                let Ok(input_token) = SplTokenAccount::unpack(&input_account.data) else {
                    continue;
                };
                if input_token.owner != *authority
                    || input_token.mint != input_mint
                    || input_token.amount < input_amount
                {
                    continue;
                }
                let output_account = finalized_account(
                    rpc,
                    derive_classic_associated_token_account(*authority, output_mint),
                )?;
                return Ok(FundedClassicAtaPair {
                    authority: *authority,
                    input_account,
                    output_account,
                });
            }
        }

        pagination_key = page.pagination_key;
        if pagination_key.is_none() {
            break;
        }
    }
    Err(format!(
        "no setup-free funded classic-SPL pair found through bounded getProgramAccountsV2 discovery; set {LIVE_TAKER} to a known public owner"
    )
    .into())
}

fn checked_classic_ata_pair(
    rpc: &RpcClient,
    authority: Pubkey,
    input_mint: Pubkey,
    output_mint: Pubkey,
    input_amount: u64,
) -> Result<Option<FundedClassicAtaPair>, Box<dyn Error>> {
    let input_address = derive_classic_associated_token_account(authority, input_mint);
    let output_address = derive_classic_associated_token_account(authority, output_mint);
    let Some(input_account) = finalized_optional_account(rpc, input_address)? else {
        eprintln!("live taker candidate rejected: authority={authority} reason=input_ata_missing");
        return Ok(None);
    };
    let Some(output_account) = finalized_optional_account(rpc, output_address)? else {
        eprintln!("live taker candidate rejected: authority={authority} reason=output_ata_missing");
        return Ok(None);
    };
    if input_account.owner != spl_token::id() || output_account.owner != spl_token::id() {
        eprintln!(
            "live taker candidate rejected: authority={authority} reason=non_classic_token_program"
        );
        return Ok(None);
    }
    let Ok(input_token) = SplTokenAccount::unpack(&input_account.data) else {
        eprintln!("live taker candidate rejected: authority={authority} reason=input_ata_decode");
        return Ok(None);
    };
    let Ok(output_token) = SplTokenAccount::unpack(&output_account.data) else {
        eprintln!("live taker candidate rejected: authority={authority} reason=output_ata_decode");
        return Ok(None);
    };
    if input_token.owner != authority
        || input_token.mint != input_mint
        || input_token.amount < input_amount
        || output_token.owner != authority
        || output_token.mint != output_mint
    {
        eprintln!(
            "live taker candidate rejected: authority={authority} reason=ata_binding_or_balance inputAmountRaw={}",
            input_token.amount
        );
        return Ok(None);
    }
    Ok(Some(FundedClassicAtaPair {
        authority,
        input_account,
        output_account,
    }))
}

fn finalized_account(rpc: &RpcClient, address: Pubkey) -> Result<Account, Box<dyn Error>> {
    finalized_optional_account(rpc, address)?
        .ok_or_else(|| format!("finalized account {address} is missing").into())
}

fn finalized_optional_account(
    rpc: &RpcClient,
    address: Pubkey,
) -> Result<Option<Account>, Box<dyn Error>> {
    Ok(rpc
        .get_account_with_commitment(&address, CommitmentConfig::finalized())?
        .value)
}

fn finalized_lookup_tables(
    rpc: &RpcClient,
    expected: &BTreeMap<String, Vec<String>>,
) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
    expected
        .iter()
        .map(|(key, expected_addresses)| {
            let key = Pubkey::from_str(key)?;
            let account = finalized_account(rpc, key)?;
            if account.owner != address_lookup_table_program::id() {
                return Err(format!("Jupiter ALT {key} has the wrong owner").into());
            }
            let table = AddressLookupTable::deserialize(&account.data)?;
            if table.meta.deactivation_slot != u64::MAX {
                return Err(format!("Jupiter ALT {key} is deactivating").into());
            }
            let addresses = table.addresses.iter().copied().collect::<Vec<_>>();
            let expected_addresses = expected_addresses
                .iter()
                .map(|address| Pubkey::from_str(address))
                .collect::<Result<Vec<_>, _>>()?;
            if addresses != expected_addresses {
                return Err(format!(
                    "Jupiter ALT {key} contents differ from finalized chain state"
                )
                .into());
            }
            Ok(AddressLookupTableAccount { key, addresses })
        })
        .collect()
}
