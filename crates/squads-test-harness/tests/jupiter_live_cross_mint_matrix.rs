use loyal_actions::{
    compile_squads_inner_instruction, derive_associated_token_account, earn_stablecoin_pairs,
    execute_program_interaction_policy_instruction,
    jupiter::{
        parse_and_validate_jupiter_exact_in_build, JupiterBuildLimits,
        JupiterExactInBuildExpectation, JupiterLookupTableSnapshot, JupiterMintSnapshot,
        JupiterTokenAccountSnapshot, JupiterV2Dialect, SOLANA_MAX_COMPUTE_UNITS,
        SOLANA_PACKET_DATA_SIZE,
    },
    EarnStablecoin,
};
use loyal_solana_env::solana_testing_keypair_from_env;
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
use spl_token_2022::{extension::StateWithExtensions, state::Account as Token2022Account};
use std::{collections::BTreeMap, env, error::Error, str::FromStr, thread::sleep, time::Duration};

const LIVE_GATE: &str = "JUPITER_LIVE_CROSS_MINT_MATRIX";
const LIVE_TAKER: &str = "JUPITER_LIVE_TAKER";
const DEFAULT_BUILD_URL: &str = "https://api.jup.ag/swap/v2/build";
const INPUT_AMOUNT_RAW: u64 = 100_000;
const MAXIMUM_SLIPPAGE_BPS: u16 = 50;

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct BuildEnvelope {
    input_mint: String,
    output_mint: String,
    in_amount: String,
    other_amount_threshold: String,
    slippage_bps: u16,
    addresses_by_lookup_table_address: BTreeMap<String, Vec<String>>,
}

#[derive(Clone)]
struct AssetSnapshot {
    asset: EarnStablecoin,
    mint: Account,
    token: Account,
    token_address: Pubkey,
}

#[derive(Default)]
struct MatrixMeasurements {
    route_v2: usize,
    shared_route_v2: usize,
    two_hop: usize,
    maximum_accounts: usize,
    maximum_raw_packet_bytes: usize,
    maximum_wrapped_packet_bytes: usize,
    maximum_units_consumed: u64,
}

#[test]
#[ignore = "requires JUPITER_LIVE_CROSS_MINT_MATRIX=1 and finalized mainnet RPC"]
fn all_canonical_directed_pairs_build_compile_fit_and_simulate_on_mainnet() {
    if env::var(LIVE_GATE).ok().as_deref() != Some("1") {
        panic!("{LIVE_GATE}=1 is required; run the explicit live matrix wrapper");
    }
    run_matrix().expect("all canonical Jupiter cross-mint routes");
}

fn run_matrix() -> Result<(), Box<dyn Error>> {
    let rpc_url = env::var("SOLANA_RPC_URL")?;
    let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::finalized());
    let authority = match env::var(LIVE_TAKER) {
        Ok(value) => Pubkey::from_str(&value)?,
        Err(env::VarError::NotPresent) => solana_testing_keypair_from_env()?.pubkey(),
        Err(error) => return Err(error.into()),
    };
    if rpc.get_balance(&authority)? < 10_000 {
        return Err("matrix taker lacks simulation fee lamports".into());
    }
    let assets = load_asset_snapshots(&rpc, authority)?;
    for snapshot in assets.values() {
        let amount = token_amount(&snapshot.token, snapshot.asset.token_program)?;
        if amount < INPUT_AMOUNT_RAW {
            return Err(format!(
                "{} source balance is {amount}, below matrix amount {INPUT_AMOUNT_RAW}",
                snapshot.asset.symbol
            )
            .into());
        }
    }

    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()?;
    let delegated_signer = Keypair::new();
    let action_account = Pubkey::new_unique();
    let mut measurements = MatrixMeasurements::default();
    let pairs = earn_stablecoin_pairs();
    for (pair_index, pair) in pairs.iter().copied().enumerate() {
        let input = assets
            .get(&pair.input_mint)
            .ok_or("canonical input snapshot missing")?;
        let output = assets
            .get(&pair.output_mint)
            .ok_or("canonical output snapshot missing")?;
        let build_bytes = fetch_build(&client, authority, input.asset, output.asset)?;
        let envelope: BuildEnvelope = serde_json::from_slice(&build_bytes)?;
        if envelope.input_mint != input.asset.mint.to_string()
            || envelope.output_mint != output.asset.mint.to_string()
            || envelope.in_amount != INPUT_AMOUNT_RAW.to_string()
            || envelope.slippage_bps > MAXIMUM_SLIPPAGE_BPS
        {
            return Err(format!(
                "{} to {} build identity differs from the request",
                input.asset.symbol, output.asset.symbol
            )
            .into());
        }
        let lookup_tables =
            finalized_lookup_tables(&rpc, &envelope.addresses_by_lookup_table_address)?;
        let expected = JupiterExactInBuildExpectation {
            authority,
            input_mint: JupiterMintSnapshot {
                address: input.asset.mint,
                owner_program: input.mint.owner,
                data: input.mint.data.clone(),
            },
            output_mint: JupiterMintSnapshot {
                address: output.asset.mint,
                owner_program: output.mint.owner,
                data: output.mint.data.clone(),
            },
            input_token_account: JupiterTokenAccountSnapshot {
                address: input.token_address,
                owner_program: input.token.owner,
                data: input.token.data.clone(),
            },
            output_token_account: JupiterTokenAccountSnapshot {
                address: output.token_address,
                owner_program: output.token.owner,
                data: output.token.data.clone(),
            },
            additional_token_accounts: assets
                .values()
                .filter(|snapshot| {
                    snapshot.asset.mint != input.asset.mint
                        && snapshot.asset.mint != output.asset.mint
                })
                .map(|snapshot| JupiterTokenAccountSnapshot {
                    address: snapshot.token_address,
                    owner_program: snapshot.token.owner,
                    data: snapshot.token.data.clone(),
                })
                .collect(),
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
        let validated = parse_and_validate_jupiter_exact_in_build(&build_bytes, &expected)
            .map_err(|error| {
                format!(
                    "{} to {} strict parser rejected current build: {error}",
                    input.asset.symbol, output.asset.symbol
                )
            })?;
        match validated.dialect {
            JupiterV2Dialect::RouteV2 => measurements.route_v2 += 1,
            JupiterV2Dialect::SharedAccountsRouteV2 => measurements.shared_route_v2 += 1,
        }
        if validated.route_step_count == 2 {
            measurements.two_hop += 1;
        }
        measurements.maximum_accounts = measurements
            .maximum_accounts
            .max(validated.structure.unique_account_count);

        let (blockhash, _) =
            rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
        let raw_instructions = vec![
            ComputeBudgetInstruction::set_compute_unit_limit(
                validated.compute_budget.simulation_unit_limit,
            ),
            ComputeBudgetInstruction::set_compute_unit_price(
                validated.compute_budget.unit_price_micro_lamports,
            ),
            validated.swap_instruction.clone(),
        ];
        let raw_transaction = unsigned_v0_transaction(
            authority,
            &raw_instructions,
            &validated.lookup_tables,
            blockhash,
        )?;
        let raw_packet_bytes = bincode::serialize(&raw_transaction)?.len();
        if raw_packet_bytes > SOLANA_PACKET_DATA_SIZE {
            return Err(format!(
                "{} to {} raw packet is {raw_packet_bytes} bytes",
                input.asset.symbol, output.asset.symbol
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
            return Err(format!(
                "{} to {} simulation failed: {error:?}",
                input.asset.symbol, output.asset.symbol
            )
            .into());
        }
        measurements.maximum_raw_packet_bytes =
            measurements.maximum_raw_packet_bytes.max(raw_packet_bytes);
        measurements.maximum_units_consumed = measurements
            .maximum_units_consumed
            .max(simulation.value.units_consumed.unwrap_or_default());

        let mut transaction_accounts = Vec::<AccountMeta>::new();
        let compiled =
            compile_squads_inner_instruction(&mut transaction_accounts, validated.swap_instruction);
        let wrapped = execute_program_interaction_policy_instruction(
            action_account,
            delegated_signer.pubkey(),
            0,
            vec![compiled],
            vec![0],
            transaction_accounts,
        );
        let wrapped_transaction = unsigned_v0_transaction(
            delegated_signer.pubkey(),
            &[
                ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
                ComputeBudgetInstruction::set_compute_unit_price(
                    validated.compute_budget.unit_price_micro_lamports,
                ),
                wrapped,
            ],
            &validated.lookup_tables,
            blockhash,
        )?;
        let wrapped_packet_bytes = bincode::serialize(&wrapped_transaction)?.len();
        if wrapped_packet_bytes > SOLANA_PACKET_DATA_SIZE {
            return Err(format!(
                "{} to {} policy-wrapped packet is {wrapped_packet_bytes} bytes",
                input.asset.symbol, output.asset.symbol
            )
            .into());
        }
        measurements.maximum_wrapped_packet_bytes = measurements
            .maximum_wrapped_packet_bytes
            .max(wrapped_packet_bytes);

        eprintln!(
            "matrix_pair={}->{} dialect={:?} hops={} accounts={} raw_packet_bytes={} wrapped_packet_bytes={} units={} simulation=ok send=false",
            input.asset.symbol,
            output.asset.symbol,
            validated.dialect,
            validated.route_step_count,
            validated.structure.unique_account_count,
            raw_packet_bytes,
            wrapped_packet_bytes,
            simulation.value.units_consumed.unwrap_or_default(),
        );
        if pair_index + 1 < pairs.len() {
            sleep(Duration::from_millis(350));
        }
    }

    eprintln!(
        "live_cross_mint_matrix PASS pairs={} route_v2={} shared_route_v2={} two_hop={} maximum_accounts={} maximum_raw_packet_bytes={} maximum_wrapped_packet_bytes={} maximum_units={} simulations={} sends=0",
        pairs.len(),
        measurements.route_v2,
        measurements.shared_route_v2,
        measurements.two_hop,
        measurements.maximum_accounts,
        measurements.maximum_raw_packet_bytes,
        measurements.maximum_wrapped_packet_bytes,
        measurements.maximum_units_consumed,
        pairs.len(),
    );
    Ok(())
}

fn load_asset_snapshots(
    rpc: &RpcClient,
    authority: Pubkey,
) -> Result<BTreeMap<Pubkey, AssetSnapshot>, Box<dyn Error>> {
    loyal_actions::earn_stablecoins()
        .iter()
        .copied()
        .map(|asset| {
            let token_address =
                derive_associated_token_account(authority, asset.mint, asset.token_program);
            let mint = finalized_account(rpc, asset.mint)?;
            let token = finalized_account(rpc, token_address)?;
            Ok((
                asset.mint,
                AssetSnapshot {
                    asset,
                    mint,
                    token,
                    token_address,
                },
            ))
        })
        .collect()
}

fn token_amount(account: &Account, token_program: Pubkey) -> Result<u64, Box<dyn Error>> {
    if token_program == spl_token::id() {
        return Ok(SplTokenAccount::unpack(&account.data)?.amount);
    }
    if token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
        return Ok(
            StateWithExtensions::<Token2022Account>::unpack(&account.data)?
                .base
                .amount,
        );
    }
    Err("unsupported canonical token program".into())
}

fn fetch_build(
    client: &reqwest::blocking::Client,
    authority: Pubkey,
    input: EarnStablecoin,
    output: EarnStablecoin,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let build_url =
        env::var("JUPITER_SWAP_BUILD_URL").unwrap_or_else(|_| DEFAULT_BUILD_URL.to_owned());
    let url = format!(
        "{build_url}?inputMint={}&outputMint={}&amount={INPUT_AMOUNT_RAW}&taker={authority}&maxAccounts=64&slippageBps={MAXIMUM_SLIPPAGE_BPS}&onlyDirectRoutes=true&dexes=AlphaQ",
        input.mint, output.mint,
    );
    for attempt in 0..5 {
        let mut request = client.get(&url);
        if let Ok(api_key) = env::var("JUPITER_API_KEY") {
            request = request.header("x-api-key", api_key);
        }
        let response = request.send()?;
        if response.status().as_u16() == 429 && attempt < 4 {
            sleep(Duration::from_secs(attempt + 1));
            continue;
        }
        return Ok(response.error_for_status()?.bytes()?.to_vec());
    }
    Err("Jupiter retry loop exhausted".into())
}

fn unsigned_v0_transaction(
    payer: Pubkey,
    instructions: &[solana_sdk::instruction::Instruction],
    lookup_tables: &[AddressLookupTableAccount],
    blockhash: solana_sdk::hash::Hash,
) -> Result<VersionedTransaction, Box<dyn Error>> {
    let message = v0::Message::try_compile(&payer, instructions, lookup_tables, blockhash)?;
    Ok(VersionedTransaction {
        signatures: vec![Signature::default(); usize::from(message.header.num_required_signatures)],
        message: VersionedMessage::V0(message),
    })
}

fn finalized_account(rpc: &RpcClient, address: Pubkey) -> Result<Account, Box<dyn Error>> {
    rpc.get_account_with_commitment(&address, CommitmentConfig::finalized())?
        .value
        .ok_or_else(|| format!("finalized account {address} is missing").into())
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
