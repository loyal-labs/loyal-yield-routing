use std::{
    env,
    fs::{self, OpenOptions},
    io::Write,
    os::unix::fs::{OpenOptionsExt, PermissionsExt},
    path::{Path, PathBuf},
    process,
    str::FromStr,
    thread,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use anyhow::{anyhow, bail, Context, Result};
use loyal_solana_env::solana_testing_keypair_from_env;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcSendTransactionConfig, RpcSimulateTransactionConfig, RpcTransactionConfig},
};
use solana_loader_v3_interface::{
    get_program_data_address,
    instruction::{create_buffer, upgrade, write},
    state::UpgradeableLoaderState,
};
use solana_sdk::{
    account::Account,
    commitment_config::{CommitmentConfig, CommitmentLevel},
    instruction::Instruction,
    message::Message,
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::{read_keypair_file, Keypair, Signature, Signer},
    transaction::Transaction,
};
use solana_transaction_status_client_types::UiTransactionEncoding;

const MAINNET_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
const UPGRADEABLE_LOADER_ID: &str = "BPFLoaderUpgradeab1e11111111111111111111111";
const EXPECTED_UPGRADE_AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const PACKET_SAFETY_MARGIN: usize = 32;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Target {
    Guard,
    VoltrRwaNavAdaptor,
}

#[derive(Clone, Copy, Debug)]
struct ProgramSpec {
    schema: &'static str,
    barrier_schema: &'static str,
    buffer_domain: &'static [u8],
    program_id: &'static str,
    artifact_filename: &'static str,
    keypair_filename: &'static str,
    elf_sha256: &'static str,
    elf_len: usize,
    max_data_len: usize,
}

const GUARD_SPEC: ProgramSpec = ProgramSpec {
    schema: "loyal-multiply-guard-deployer/v2",
    barrier_schema: "loyal-multiply-guard-mainnet-barrier/v2",
    buffer_domain: b"loyal-multiply-guard-upgradeable-buffer-v2\0",
    program_id: "8moAa3vXstMPop9FtEnhTDRmcyo9HPn1CsywGMZ9K9n8",
    artifact_filename: "loyal_multiply_guard_program.so",
    keypair_filename: "loyal_multiply_guard_program-keypair.json",
    elf_sha256: "60ba04c8ef75a4a67923db99464fdcd409ee9b1b9b0f6424cf67a7647f3a5a75",
    elf_len: 194_416,
    max_data_len: 194_416,
};

const ADAPTOR_SPEC: ProgramSpec = ProgramSpec {
    schema: "loyal-voltr-rwa-nav-adaptor-deployer/v2",
    barrier_schema: "loyal-voltr-rwa-nav-adaptor-mainnet-barrier/v2",
    buffer_domain: b"loyal-voltr-rwa-nav-adaptor-upgradeable-buffer-v2\0",
    program_id: "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW",
    artifact_filename: "loyal_voltr_rwa_nav_adaptor.so",
    keypair_filename: "loyal_voltr_rwa_nav_adaptor-keypair.json",
    elf_sha256: "59d4d883324712c1dd849787f9475bba1431c2940e82bb6e37d3fd41471140d4",
    elf_len: 93_480,
    max_data_len: 115_384,
};

#[derive(Debug, Serialize)]
struct ArtifactReport {
    bytes: usize,
    sha256: String,
    max_data_len: usize,
}

#[derive(Debug, Serialize)]
struct BufferReport {
    address: String,
    state_before: String,
    data_bytes: usize,
    rent_lamports: u64,
    write_chunk_bytes: usize,
    total_write_transactions: usize,
    pending_write_transactions_before: usize,
    submitted_write_transactions: usize,
}

#[derive(Debug, Serialize)]
struct RentReport {
    buffer_lamports: u64,
    program_lamports: u64,
    programdata_lamports: u64,
    minimum_starting_balance_lamports: u64,
}

#[derive(Clone, Debug, Serialize)]
struct VerificationReport {
    finalized_slot: u64,
    programdata_deployment_slot: u64,
    program_executable: bool,
    program_owner: String,
    programdata_owner: String,
    programdata_bytes: usize,
    deployed_programdata_payload_sha256: String,
    deployed_artifact_prefix_sha256: String,
    upgrade_authority: String,
}

#[derive(Debug, Serialize)]
struct DeploymentReport {
    schema: &'static str,
    mode: &'static str,
    cluster: &'static str,
    genesis_hash: String,
    status: String,
    payer: String,
    payer_balance_lamports_before: u64,
    payer_balance_lamports_after: u64,
    program_id: String,
    programdata_address: String,
    artifact: ArtifactReport,
    buffer: BufferReport,
    rent: RentReport,
    create_buffer_signature: Option<String>,
    deploy_signature: Option<String>,
    upgrade_signature: Option<String>,
    transactions: Vec<StageReceiptReport>,
    verification: Option<VerificationReport>,
}

#[derive(Clone, Debug, Serialize)]
struct StageReceiptReport {
    stage: String,
    signature: String,
    transaction_sha256: String,
    message_sha256: String,
    barrier_path: String,
    simulation_context_slot: u64,
    finalized_slot: u64,
    reconciled_from_barrier: bool,
}

#[derive(Debug, Deserialize, Serialize)]
struct StageBarrier {
    schema: String,
    artifact_sha256: String,
    program_id: String,
    payer: String,
    stage: String,
    intent_sha256: String,
    signature: String,
    transaction_sha256: String,
    message_sha256: String,
    wire_hex: String,
    simulation_context_slot: u64,
    simulation_units_consumed: Option<u64>,
    simulation_logs_sha256: String,
    recent_blockhash: String,
    last_valid_block_height: u64,
    created_at_unix_seconds: u64,
}

#[derive(Debug, Serialize)]
struct PublicError<'a> {
    schema: &'static str,
    status: &'static str,
    error: &'a str,
}

#[derive(Debug)]
struct Args {
    execute: bool,
    barrier_dir: Option<PathBuf>,
    target: Target,
}

fn main() {
    let rpc_url = env::var("SOLANA_RPC_URL").unwrap_or_default();
    match run(&rpc_url) {
        Ok(report) => {
            println!(
                "{}",
                serde_json::to_string_pretty(&report).expect("serialize public deployment report")
            );
        }
        Err(error) => {
            let mut message = format!("{error:#}");
            if !rpc_url.is_empty() {
                message = message.replace(&rpc_url, "[redacted-rpc]");
            }
            let public = PublicError {
                schema: "loyal-pinned-program-deployer-error/v1",
                status: "error",
                error: &message,
            };
            eprintln!(
                "{}",
                serde_json::to_string_pretty(&public).expect("serialize public error")
            );
            process::exit(1);
        }
    }
}

fn run(rpc_url: &str) -> Result<DeploymentReport> {
    let args = parse_args()?;
    let spec = match args.target {
        Target::Guard => GUARD_SPEC,
        Target::VoltrRwaNavAdaptor => ADAPTOR_SPEC,
    };
    if rpc_url.trim().is_empty() {
        bail!("SOLANA_RPC_URL is required");
    }
    let expected_authority = Pubkey::from_str(EXPECTED_UPGRADE_AUTHORITY)?;
    if args.execute && env::var("CONFIRM_MAINNET").as_deref() != Ok("1") {
        bail!("--execute requires CONFIRM_MAINNET=1");
    }

    let artifact_path = artifact_path(&spec);
    let artifact = fs::read(&artifact_path).context("read pinned program artifact")?;
    validate_artifact(&artifact, &spec)?;
    let program_id = Pubkey::from_str(spec.program_id)?;
    let rpc = RpcClient::new_with_commitment(rpc_url.to_owned(), CommitmentConfig::confirmed());
    let genesis_hash = rpc.get_genesis_hash().context("read RPC genesis hash")?;
    if genesis_hash.to_string() != MAINNET_GENESIS_HASH {
        bail!("refusing deployment: RPC is not Solana mainnet-beta");
    }

    let payer_balance_before = rpc
        .get_balance_with_commitment(&expected_authority, CommitmentConfig::finalized())
        .context("read finalized payer balance")?
        .value;
    let programdata_address = get_program_data_address(&program_id);
    // Transaction size is independent of the concrete 32-byte buffer address.
    // Dry-run deliberately does not load signer material merely to derive that address.
    let write_chunk_bytes = calculate_write_chunk_size(&expected_authority, &programdata_address)?;
    let total_write_transactions = artifact.len().div_ceil(write_chunk_bytes);

    let buffer_data_len = UpgradeableLoaderState::size_of_buffer(artifact.len());
    let program_len = UpgradeableLoaderState::size_of_program();
    let programdata_len = UpgradeableLoaderState::size_of_programdata(spec.max_data_len);
    let buffer_rent = rpc
        .get_minimum_balance_for_rent_exemption(buffer_data_len)
        .context("read buffer rent")?;
    let program_rent = rpc
        .get_minimum_balance_for_rent_exemption(program_len)
        .context("read Program rent")?;
    let programdata_rent = rpc
        .get_minimum_balance_for_rent_exemption(programdata_len)
        .context("read ProgramData rent")?;
    let full_deployment_fee_reserve = (total_write_transactions as u64 + 2).saturating_mul(10_000);
    let full_deployment_minimum = program_rent
        .saturating_add(programdata_rent)
        .saturating_add(full_deployment_fee_reserve);

    let deployed =
        inspect_deployed_program(&rpc, &program_id, &expected_authority, &artifact, &spec)?;
    if let Some((verification, true)) = deployed.as_ref() {
        let payer_balance_after = rpc
            .get_balance_with_commitment(&expected_authority, CommitmentConfig::finalized())?
            .value;
        return Ok(DeploymentReport {
            schema: spec.schema,
            mode: if args.execute { "execute" } else { "dry-run" },
            cluster: "mainnet-beta",
            genesis_hash: genesis_hash.to_string(),
            status: "already-deployed-and-verified".to_owned(),
            payer: expected_authority.to_string(),
            payer_balance_lamports_before: payer_balance_before,
            payer_balance_lamports_after: payer_balance_after,
            program_id: program_id.to_string(),
            programdata_address: programdata_address.to_string(),
            artifact: ArtifactReport {
                bytes: artifact.len(),
                sha256: spec.elf_sha256.to_owned(),
                max_data_len: spec.max_data_len,
            },
            buffer: BufferReport {
                address: "not-loaded-without-signer".to_owned(),
                state_before: "consumed-or-not-required".to_owned(),
                data_bytes: artifact.len(),
                rent_lamports: buffer_rent,
                write_chunk_bytes,
                total_write_transactions,
                pending_write_transactions_before: 0,
                submitted_write_transactions: 0,
            },
            rent: RentReport {
                buffer_lamports: buffer_rent,
                program_lamports: program_rent,
                programdata_lamports: programdata_rent,
                minimum_starting_balance_lamports: full_deployment_minimum,
            },
            create_buffer_signature: None,
            deploy_signature: None,
            upgrade_signature: None,
            transactions: Vec::new(),
            verification: Some(verification.clone()),
        });
    }
    let is_upgrade = deployed.is_some();

    if !args.execute {
        let remaining_fee_reserve = (total_write_transactions as u64 + 2).saturating_mul(10_000);
        let minimum_starting_balance = if is_upgrade {
            buffer_rent.saturating_add(remaining_fee_reserve)
        } else {
            program_rent
                .saturating_add(programdata_rent)
                .saturating_add(remaining_fee_reserve)
        };
        return Ok(DeploymentReport {
            schema: spec.schema,
            mode: "dry-run",
            cluster: "mainnet-beta",
            genesis_hash: genesis_hash.to_string(),
            status: "planned-no-broadcast".to_owned(),
            payer: expected_authority.to_string(),
            payer_balance_lamports_before: payer_balance_before,
            payer_balance_lamports_after: payer_balance_before,
            program_id: program_id.to_string(),
            programdata_address: programdata_address.to_string(),
            artifact: ArtifactReport {
                bytes: artifact.len(),
                sha256: spec.elf_sha256.to_owned(),
                max_data_len: spec.max_data_len,
            },
            buffer: BufferReport {
                address: "derived-only-after-execute-confirmation".to_owned(),
                state_before: "not-inspected-without-signer".to_owned(),
                data_bytes: artifact.len(),
                rent_lamports: buffer_rent,
                write_chunk_bytes,
                total_write_transactions,
                pending_write_transactions_before: total_write_transactions,
                submitted_write_transactions: 0,
            },
            rent: RentReport {
                buffer_lamports: buffer_rent,
                program_lamports: program_rent,
                programdata_lamports: programdata_rent,
                minimum_starting_balance_lamports: minimum_starting_balance,
            },
            create_buffer_signature: None,
            deploy_signature: None,
            upgrade_signature: None,
            transactions: Vec::new(),
            verification: deployed
                .as_ref()
                .map(|(verification, _matches)| verification.clone()),
        });
    }

    let barrier_dir = args
        .barrier_dir
        .as_deref()
        .ok_or_else(|| anyhow!("--execute requires --barrier-dir PATH"))?;
    prepare_barrier_dir(barrier_dir)?;
    let payer = solana_testing_keypair_from_env().context("load SOLANA_TESTING_PK")?;
    if payer.pubkey() != expected_authority {
        bail!("SOLANA_TESTING_PK does not match the pinned upgrade authority");
    }
    let elf_hash = sha256_bytes(&artifact);
    let buffer_keypair = derive_buffer_keypair(&payer, &program_id, &elf_hash, &spec);
    let buffer_address = buffer_keypair.pubkey();
    let write_chunk_bytes = calculate_write_chunk_size(&payer.pubkey(), &buffer_address)?;
    let initial_buffer = read_buffer(
        &rpc,
        &buffer_address,
        &payer.pubkey(),
        CommitmentConfig::finalized(),
    )?;
    let state_before = if initial_buffer.is_some() {
        "resumable"
    } else {
        "absent"
    };
    let pending_before = pending_writes(
        &artifact,
        initial_buffer
            .as_ref()
            .map(|account| account.data.as_slice()),
        &buffer_address,
        &payer.pubkey(),
        write_chunk_bytes,
    )?
    .len();
    let buffer_lamport_credit = initial_buffer
        .as_ref()
        .map(|account| account.lamports)
        .unwrap_or(0);
    let remaining_transaction_count = pending_before as u64
        + 1 // DeployWithMaxDataLen or Upgrade.
        + u64::from(initial_buffer.is_none()); // Create + initialize buffer.
    let remaining_fee_reserve = remaining_transaction_count.saturating_mul(10_000);
    let minimum_starting_balance = if is_upgrade {
        buffer_rent
            .saturating_sub(buffer_lamport_credit)
            .saturating_add(remaining_fee_reserve)
    } else {
        program_rent
            .saturating_add(programdata_rent)
            .saturating_sub(buffer_lamport_credit)
            .saturating_add(remaining_fee_reserve)
    };

    if payer_balance_before < minimum_starting_balance {
        bail!(
            "payer balance is below the conservative deployment minimum of {minimum_starting_balance} lamports"
        );
    }

    let mut transactions = Vec::new();
    let mut minimum_context_slot = deployed
        .as_ref()
        .map(|(verification, _)| verification.finalized_slot)
        .unwrap_or(rpc.get_slot_with_commitment(CommitmentConfig::finalized())?);
    let mut create_buffer_signature = None;
    if initial_buffer.is_none() {
        let instructions = create_buffer(
            &payer.pubkey(),
            &buffer_address,
            &payer.pubkey(),
            buffer_rent,
            artifact.len(),
        )
        .map_err(|error| anyhow!("build buffer creation instructions: {error}"))?;
        let receipt = execute_exact_stage(
            &rpc,
            barrier_dir,
            &spec,
            "create_buffer",
            &instructions,
            &payer.pubkey(),
            &[&payer, &buffer_keypair],
            minimum_context_slot,
        )?;
        minimum_context_slot = receipt.finalized_slot;
        create_buffer_signature = Some(receipt.signature.clone());
        transactions.push(receipt);
        wait_for_finalized_buffer(
            &rpc,
            &buffer_address,
            &payer.pubkey(),
            Duration::from_secs(60),
        )?
        .ok_or_else(|| anyhow!("finalized buffer is unavailable after creation"))?;
    }

    let buffer = read_buffer(
        &rpc,
        &buffer_address,
        &payer.pubkey(),
        CommitmentConfig::finalized(),
    )?
    .ok_or_else(|| anyhow!("buffer is unavailable after creation"))?;
    let pending = pending_writes(
        &artifact,
        Some(&buffer.data),
        &buffer_address,
        &payer.pubkey(),
        write_chunk_bytes,
    )?;
    let submitted_write_transactions = pending.len();
    for (position, pending_write) in pending.iter().enumerate() {
        let stage = format!(
            "write_{:06}_{}",
            pending_write.offset,
            pending_write.bytes.len()
        );
        let receipt = execute_exact_stage(
            &rpc,
            barrier_dir,
            &spec,
            &stage,
            std::slice::from_ref(&pending_write.instruction),
            &payer.pubkey(),
            &[&payer],
            minimum_context_slot,
        )?;
        minimum_context_slot = receipt.finalized_slot;
        let finalized_buffer = wait_for_finalized_buffer_chunk(
            &rpc,
            &buffer_address,
            &payer.pubkey(),
            pending_write.offset,
            &pending_write.bytes,
            Duration::from_secs(60),
        )?
        .ok_or_else(|| anyhow!("finalized buffer CAS failed after {stage}"))?;
        let _ = finalized_buffer;
        transactions.push(receipt);
        eprintln!(
            "guard buffer write {}/{} finalized",
            position + 1,
            pending.len()
        );
    }

    let complete_buffer = read_buffer(
        &rpc,
        &buffer_address,
        &payer.pubkey(),
        CommitmentConfig::finalized(),
    )?
    .ok_or_else(|| anyhow!("buffer disappeared before deployment"))?;
    let metadata_len = UpgradeableLoaderState::size_of_buffer_metadata();
    if complete_buffer.data[metadata_len..] != artifact {
        bail!("confirmed buffer bytes do not match the pinned artifact; rerun to resume writes");
    }

    if is_upgrade {
        let current =
            inspect_deployed_program(&rpc, &program_id, &payer.pubkey(), &artifact, &spec)?
                .ok_or_else(|| anyhow!("guard program disappeared before upgrade"))?;
        let initial = deployed
            .as_ref()
            .map(|(verification, _)| verification)
            .ok_or_else(|| anyhow!("missing initial guard deployment state"))?;
        if current.0.programdata_deployment_slot != initial.programdata_deployment_slot
            || current.0.deployed_programdata_payload_sha256
                != initial.deployed_programdata_payload_sha256
        {
            bail!("guard ProgramData changed while the upgrade buffer was being prepared");
        }
        let receipt = execute_exact_stage(
            &rpc,
            barrier_dir,
            &spec,
            "upgrade_program",
            &[upgrade(
                &program_id,
                &buffer_address,
                &payer.pubkey(),
                &payer.pubkey(),
            )],
            &payer.pubkey(),
            &[&payer],
            minimum_context_slot,
        )?;
        let upgrade_signature = receipt.signature.clone();
        transactions.push(receipt);
        let (verification, matches) =
            inspect_deployed_program(&rpc, &program_id, &payer.pubkey(), &artifact, &spec)?
                .ok_or_else(|| anyhow!("finalized guard program disappeared after upgrade"))?;
        if !matches {
            bail!("finalized ProgramData bytes do not match the pinned upgraded artifact");
        }
        let payer_balance_after = rpc
            .get_balance_with_commitment(&payer.pubkey(), CommitmentConfig::finalized())?
            .value;
        return Ok(DeploymentReport {
            schema: spec.schema,
            mode: "execute",
            cluster: "mainnet-beta",
            genesis_hash: genesis_hash.to_string(),
            status: "upgraded-and-verified".to_owned(),
            payer: payer.pubkey().to_string(),
            payer_balance_lamports_before: payer_balance_before,
            payer_balance_lamports_after: payer_balance_after,
            program_id: program_id.to_string(),
            programdata_address: programdata_address.to_string(),
            artifact: ArtifactReport {
                bytes: artifact.len(),
                sha256: spec.elf_sha256.to_owned(),
                max_data_len: spec.max_data_len,
            },
            buffer: BufferReport {
                address: buffer_address.to_string(),
                state_before: state_before.to_owned(),
                data_bytes: artifact.len(),
                rent_lamports: buffer_rent,
                write_chunk_bytes,
                total_write_transactions,
                pending_write_transactions_before: pending_before,
                submitted_write_transactions,
            },
            rent: RentReport {
                buffer_lamports: buffer_rent,
                program_lamports: program_rent,
                programdata_lamports: programdata_rent,
                minimum_starting_balance_lamports: minimum_starting_balance,
            },
            create_buffer_signature,
            deploy_signature: None,
            upgrade_signature: Some(upgrade_signature),
            transactions,
            verification: Some(verification),
        });
    }

    if rpc
        .get_account_with_commitment(&program_id, CommitmentConfig::finalized())?
        .value
        .is_some()
    {
        bail!("guard program account appeared before deploy; rerun to reconcile");
    }
    let program_keypair_path = program_keypair_path(&spec);
    validate_private_key_file(&program_keypair_path)?;
    let program_keypair = read_keypair_file(&program_keypair_path)
        .map_err(|_| anyhow!("read existing guard program keypair"))?;
    if program_keypair.pubkey() != program_id {
        bail!("existing program keypair does not match the pinned guard program id");
    }
    let deploy_instructions = build_deploy_instructions(
        &payer.pubkey(),
        &program_id,
        &buffer_address,
        &payer.pubkey(),
        program_rent,
        spec.max_data_len,
    )
    .map_err(|error| anyhow!("build deploy instructions: {error}"))?;
    let receipt = execute_exact_stage(
        &rpc,
        barrier_dir,
        &spec,
        "deploy_program",
        &deploy_instructions,
        &payer.pubkey(),
        &[&payer, &program_keypair],
        minimum_context_slot,
    )?;
    let deploy_signature = receipt.signature.clone();
    transactions.push(receipt);
    let (verification, matches) =
        inspect_deployed_program(&rpc, &program_id, &payer.pubkey(), &artifact, &spec)?
            .ok_or_else(|| anyhow!("finalized guard program is missing after deployment"))?;
    if !matches {
        bail!("finalized ProgramData bytes do not match the pinned deployed artifact");
    }
    let payer_balance_after = rpc
        .get_balance_with_commitment(&payer.pubkey(), CommitmentConfig::finalized())?
        .value;

    Ok(DeploymentReport {
        schema: spec.schema,
        mode: "execute",
        cluster: "mainnet-beta",
        genesis_hash: genesis_hash.to_string(),
        status: "deployed-and-verified".to_owned(),
        payer: payer.pubkey().to_string(),
        payer_balance_lamports_before: payer_balance_before,
        payer_balance_lamports_after: payer_balance_after,
        program_id: program_id.to_string(),
        programdata_address: programdata_address.to_string(),
        artifact: ArtifactReport {
            bytes: artifact.len(),
            sha256: spec.elf_sha256.to_owned(),
            max_data_len: spec.max_data_len,
        },
        buffer: BufferReport {
            address: buffer_address.to_string(),
            state_before: state_before.to_owned(),
            data_bytes: artifact.len(),
            rent_lamports: buffer_rent,
            write_chunk_bytes,
            total_write_transactions,
            pending_write_transactions_before: pending_before,
            submitted_write_transactions,
        },
        rent: RentReport {
            buffer_lamports: buffer_rent,
            program_lamports: program_rent,
            programdata_lamports: programdata_rent,
            minimum_starting_balance_lamports: minimum_starting_balance,
        },
        create_buffer_signature,
        deploy_signature: Some(deploy_signature),
        upgrade_signature: None,
        transactions,
        verification: Some(verification),
    })
}

fn parse_args() -> Result<Args> {
    let mut execute = false;
    let mut barrier_dir = None;
    let mut target = None;
    let mut arguments = env::args().skip(1);
    while let Some(argument) = arguments.next() {
        match argument.as_str() {
            "--execute" if !execute => execute = true,
            "--barrier-dir" if barrier_dir.is_none() => {
                let value = arguments
                    .next()
                    .ok_or_else(|| anyhow!("--barrier-dir requires a path"))?;
                if value.starts_with("--") {
                    bail!("--barrier-dir requires a path");
                }
                barrier_dir = Some(PathBuf::from(value));
            }
            "--target" if target.is_none() => {
                target = Some(match arguments.next().as_deref() {
                    Some("guard") => Target::Guard,
                    Some("voltr-rwa-nav-adaptor") => Target::VoltrRwaNavAdaptor,
                    Some(value) => bail!("unsupported --target {value}"),
                    None => bail!("--target requires guard or voltr-rwa-nav-adaptor"),
                });
            }
            "--help" | "-h" => {
                println!(
                    "Usage: loyal-multiply-guard-deployer --target guard|voltr-rwa-nav-adaptor [--execute --barrier-dir ABSOLUTE_PATH]\n\nDry-run is the default and does not load signer material. --execute additionally requires CONFIRM_MAINNET=1 and persists one-send barriers before any broadcast."
                );
                process::exit(0);
            }
            "--execute" => bail!("--execute may only be provided once"),
            "--barrier-dir" => bail!("--barrier-dir may only be provided once"),
            "--target" => bail!("--target may only be provided once"),
            _ => bail!("unknown argument: {argument}"),
        }
    }
    if execute && barrier_dir.is_none() {
        bail!("--execute requires --barrier-dir PATH");
    }
    Ok(Args {
        execute,
        barrier_dir,
        target: target.ok_or_else(|| anyhow!("--target is required"))?,
    })
}

fn artifact_path(spec: &ProgramSpec) -> PathBuf {
    if let Some(path) = env::var_os("LOYAL_PINNED_ARTIFACT_PATH") {
        return PathBuf::from(path);
    }
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../target/deploy")
        .join(spec.artifact_filename)
}

fn program_keypair_path(spec: &ProgramSpec) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../target/deploy")
        .join(spec.keypair_filename)
}

fn validate_private_key_file(path: &Path) -> Result<()> {
    let metadata = fs::metadata(path).context("inspect existing guard program keypair")?;
    if !metadata.is_file() {
        bail!("existing guard program keypair path is not a file");
    }
    if metadata.permissions().mode() & 0o077 != 0 {
        bail!("existing guard program keypair must not be group- or world-readable");
    }
    Ok(())
}

fn validate_artifact(artifact: &[u8], spec: &ProgramSpec) -> Result<()> {
    if artifact.len() != spec.elf_len {
        bail!(
            "pinned artifact length mismatch: expected {}, got {}",
            spec.elf_len,
            artifact.len()
        );
    }
    let observed = hex_sha256(artifact);
    if observed != spec.elf_sha256 {
        bail!("artifact SHA-256 does not match the pinned deployable");
    }
    Ok(())
}

#[allow(deprecated)]
fn build_deploy_instructions(
    payer: &Pubkey,
    program: &Pubkey,
    buffer: &Pubkey,
    authority: &Pubkey,
    program_lamports: u64,
    max_data_len: usize,
) -> Result<Vec<solana_sdk::instruction::Instruction>, solana_sdk::instruction::InstructionError> {
    solana_loader_v3_interface::instruction::deploy_with_max_program_len(
        payer,
        program,
        buffer,
        authority,
        program_lamports,
        max_data_len,
    )
}

fn upgradeable_loader_id() -> Pubkey {
    Pubkey::from_str(UPGRADEABLE_LOADER_ID).expect("valid upgradeable loader id")
}

fn derive_buffer_keypair(
    payer: &Keypair,
    program_id: &Pubkey,
    elf_hash: &[u8; 32],
    spec: &ProgramSpec,
) -> Keypair {
    let payer_bytes = payer.to_bytes();
    let mut hasher = Sha256::new();
    hasher.update(spec.buffer_domain);
    hasher.update(&payer_bytes[..32]);
    hasher.update(program_id.as_ref());
    hasher.update(elf_hash);
    hasher.update((spec.max_data_len as u64).to_le_bytes());
    let seed: [u8; 32] = hasher.finalize().into();
    Keypair::new_from_array(seed)
}

fn calculate_write_chunk_size(payer: &Pubkey, buffer: &Pubkey) -> Result<usize> {
    let max_packet = PACKET_DATA_SIZE.saturating_sub(PACKET_SAFETY_MARGIN);
    let mut low = 1usize;
    let mut high = 1_100usize;
    while low < high {
        let candidate = (low + high + 1) / 2;
        let instruction = write(buffer, payer, 0, vec![0u8; candidate]);
        let transaction = Transaction::new_unsigned(Message::new(&[instruction], Some(payer)));
        if bincode::serialize(&transaction)?.len() <= max_packet {
            low = candidate;
        } else {
            high = candidate - 1;
        }
    }
    if low == 0 {
        bail!("could not construct a packet-safe loader write transaction");
    }
    Ok(low)
}

struct PendingWrite {
    offset: usize,
    bytes: Vec<u8>,
    instruction: Instruction,
}

fn pending_writes(
    artifact: &[u8],
    buffer_data: Option<&[u8]>,
    buffer: &Pubkey,
    authority: &Pubkey,
    chunk_size: usize,
) -> Result<Vec<PendingWrite>> {
    let metadata_len = UpgradeableLoaderState::size_of_buffer_metadata();
    let existing = match buffer_data {
        Some(data) => {
            if data.len() != UpgradeableLoaderState::size_of_buffer(artifact.len()) {
                bail!("resumable buffer has the wrong data length");
            }
            Some(&data[metadata_len..])
        }
        None => None,
    };
    let mut writes = Vec::new();
    for (chunk_index, bytes) in artifact.chunks(chunk_size).enumerate() {
        let offset = chunk_index * chunk_size;
        if existing.and_then(|data| data.get(offset..offset + bytes.len())) == Some(bytes) {
            continue;
        }
        let wire_offset = u32::try_from(offset).context("loader write offset exceeds u32")?;
        let instruction = write(buffer, authority, wire_offset, bytes.to_vec());
        let message = Message::new(std::slice::from_ref(&instruction), Some(authority));
        let transaction = Transaction::new_unsigned(message.clone());
        let packet_bytes = bincode::serialize(&transaction)?.len();
        if packet_bytes > PACKET_DATA_SIZE {
            bail!("loader write transaction exceeds packet limit");
        }
        writes.push(PendingWrite {
            offset,
            bytes: bytes.to_vec(),
            instruction,
        });
    }
    Ok(writes)
}

fn read_buffer(
    rpc: &RpcClient,
    buffer: &Pubkey,
    authority: &Pubkey,
    commitment: CommitmentConfig,
) -> Result<Option<Account>> {
    let response = rpc
        .get_account_with_commitment(buffer, commitment)
        .context("read deterministic loader buffer")?;
    let Some(account) = response.value else {
        return Ok(None);
    };
    if account.owner != upgradeable_loader_id() || account.executable {
        bail!("deterministic buffer address is occupied by an unexpected account");
    }
    if account.data.len() < UpgradeableLoaderState::size_of_buffer_metadata() {
        bail!("deterministic buffer account is too short");
    }
    let state: UpgradeableLoaderState =
        bincode::deserialize(&account.data[..UpgradeableLoaderState::size_of_buffer_metadata()])
            .context("decode deterministic buffer state")?;
    match state {
        UpgradeableLoaderState::Buffer {
            authority_address: Some(observed),
        } if observed == *authority => Ok(Some(account)),
        _ => bail!("deterministic buffer is not controlled by the pinned upgrade authority"),
    }
}

fn wait_for_finalized_buffer(
    rpc: &RpcClient,
    buffer: &Pubkey,
    authority: &Pubkey,
    timeout: Duration,
) -> Result<Option<Account>> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(account) = read_buffer(rpc, buffer, authority, CommitmentConfig::finalized())? {
            return Ok(Some(account));
        }
        if Instant::now() >= deadline {
            return Ok(None);
        }
        thread::sleep(Duration::from_millis(500));
    }
}

fn wait_for_finalized_buffer_chunk(
    rpc: &RpcClient,
    buffer: &Pubkey,
    authority: &Pubkey,
    offset: usize,
    expected: &[u8],
    timeout: Duration,
) -> Result<Option<Account>> {
    let metadata_len = UpgradeableLoaderState::size_of_buffer_metadata();
    let start = metadata_len + offset;
    let end = start + expected.len();
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(account) = read_buffer(rpc, buffer, authority, CommitmentConfig::finalized())? {
            if account.data.get(start..end) == Some(expected) {
                return Ok(Some(account));
            }
        }
        if Instant::now() >= deadline {
            return Ok(None);
        }
        thread::sleep(Duration::from_millis(500));
    }
}

fn prepare_barrier_dir(path: &Path) -> Result<()> {
    if !path.is_absolute() || !path.starts_with("/private/tmp") {
        bail!("barrier directory must be an absolute path under /private/tmp");
    }
    if !path.exists() {
        fs::create_dir_all(path).context("create deployment barrier directory")?;
    }
    let metadata = fs::symlink_metadata(path).context("inspect deployment barrier directory")?;
    if !metadata.file_type().is_dir() || metadata.file_type().is_symlink() {
        bail!("deployment barrier path must be a real directory");
    }
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))
        .context("restrict deployment barrier directory permissions")?;
    Ok(())
}

fn decode_hex(value: &str) -> Result<Vec<u8>> {
    if !value.len().is_multiple_of(2) || !value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        bail!("persisted transaction hex is malformed");
    }
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let text = std::str::from_utf8(pair).expect("validated ASCII hex");
            u8::from_str_radix(text, 16).context("decode persisted transaction hex")
        })
        .collect()
}

fn transaction_from_finalized(
    rpc: &RpcClient,
    signature: &Signature,
    expected_wire: &[u8],
    stage: &str,
) -> Result<u64> {
    let finalized = rpc
        .get_transaction_with_config(
            signature,
            RpcTransactionConfig {
                encoding: Some(UiTransactionEncoding::Base64),
                commitment: Some(CommitmentConfig::finalized()),
                max_supported_transaction_version: Some(0),
            },
        )
        .with_context(|| format!("read finalized {stage} transaction"))?;
    let decoded = finalized
        .transaction
        .transaction
        .decode()
        .ok_or_else(|| anyhow!("finalized {stage} transaction bytes did not decode"))?;
    if bincode::serialize(&decoded)? != expected_wire {
        bail!("finalized {stage} transaction differs from the persisted signed wire");
    }
    if decoded.signatures.first() != Some(signature) {
        bail!("finalized {stage} signature differs from persisted identity");
    }
    let meta = finalized
        .transaction
        .meta
        .as_ref()
        .ok_or_else(|| anyhow!("finalized {stage} transaction omitted metadata"))?;
    if let Some(error) = &meta.err {
        bail!("{stage} finalized with error: {error:?}");
    }
    Ok(finalized.slot)
}

fn execute_exact_stage(
    rpc: &RpcClient,
    barrier_dir: &Path,
    spec: &ProgramSpec,
    stage: &str,
    instructions: &[Instruction],
    payer: &Pubkey,
    signers: &[&Keypair],
    minimum_context_slot: u64,
) -> Result<StageReceiptReport> {
    if stage.is_empty()
        || !stage
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'-')
    {
        bail!("deployment stage name is not path-safe");
    }
    let program_id = Pubkey::from_str(spec.program_id)?;
    let intent_sha256 = hex_sha256(&bincode::serialize(&(
        spec.program_id,
        spec.elf_sha256,
        stage,
        payer,
        instructions,
    ))?);
    let barrier_path = barrier_dir.join(format!("{stage}.json"));
    if barrier_path.exists() {
        let bytes = fs::read(&barrier_path).context("read existing deployment barrier")?;
        let barrier: StageBarrier =
            serde_json::from_slice(&bytes).context("decode existing deployment barrier")?;
        if barrier.schema != spec.barrier_schema
            || barrier.artifact_sha256 != spec.elf_sha256
            || barrier.program_id != spec.program_id
            || barrier.payer != payer.to_string()
            || barrier.stage != stage
            || barrier.intent_sha256 != intent_sha256
        {
            bail!("existing {stage} barrier does not match the exact deployment intent");
        }
        let wire = decode_hex(&barrier.wire_hex)?;
        if hex_sha256(&wire) != barrier.transaction_sha256 {
            bail!("existing {stage} barrier signed-wire hash drifted");
        }
        let transaction: Transaction =
            bincode::deserialize(&wire).context("decode existing signed deployment wire")?;
        let signature = Signature::from_str(&barrier.signature)
            .context("decode existing deployment signature")?;
        if transaction.signatures.first() != Some(&signature)
            || hex_sha256(&transaction.message_data()) != barrier.message_sha256
        {
            bail!("existing {stage} barrier message or signature drifted");
        }
        let finalized_slot = transaction_from_finalized(rpc, &signature, &wire, stage)
            .with_context(|| {
                format!(
                    "{stage} barrier exists but is not a finalized success; blind resend forbidden"
                )
            })?;
        return Ok(StageReceiptReport {
            stage: stage.to_owned(),
            signature: signature.to_string(),
            transaction_sha256: barrier.transaction_sha256,
            message_sha256: barrier.message_sha256,
            barrier_path: barrier_path.display().to_string(),
            simulation_context_slot: barrier.simulation_context_slot,
            finalized_slot,
            reconciled_from_barrier: true,
        });
    }

    let (blockhash, last_valid_block_height) = rpc
        .get_latest_blockhash_with_commitment(CommitmentConfig::confirmed())
        .context("read deployment blockhash")?;
    let mut transaction = Transaction::new_unsigned(Message::new(instructions, Some(payer)));
    transaction
        .try_sign(signers, blockhash)
        .context("sign exact deployment stage")?;
    let wire = bincode::serialize(&transaction)?;
    if wire.len() > PACKET_DATA_SIZE {
        bail!("signed {stage} transaction exceeds the Solana packet limit");
    }
    let signature = *transaction
        .signatures
        .first()
        .ok_or_else(|| anyhow!("signed {stage} transaction has no signature"))?;
    let simulation = rpc
        .simulate_transaction_with_config(
            &transaction,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                replace_recent_blockhash: false,
                commitment: Some(CommitmentConfig::confirmed()),
                min_context_slot: Some(minimum_context_slot),
                ..RpcSimulateTransactionConfig::default()
            },
        )
        .with_context(|| format!("simulate exact {stage} transaction"))?;
    if let Some(error) = simulation.value.err {
        bail!("signed {stage} simulation failed: {error:?}");
    }
    let logs = simulation.value.logs.unwrap_or_default().join("\n");
    let barrier = StageBarrier {
        schema: spec.barrier_schema.to_owned(),
        artifact_sha256: spec.elf_sha256.to_owned(),
        program_id: program_id.to_string(),
        payer: payer.to_string(),
        stage: stage.to_owned(),
        intent_sha256,
        signature: signature.to_string(),
        transaction_sha256: hex_sha256(&wire),
        message_sha256: hex_sha256(&transaction.message_data()),
        wire_hex: wire.iter().map(|byte| format!("{byte:02x}")).collect(),
        simulation_context_slot: simulation.context.slot,
        simulation_units_consumed: simulation.value.units_consumed,
        simulation_logs_sha256: hex_sha256(logs.as_bytes()),
        recent_blockhash: blockhash.to_string(),
        last_valid_block_height,
        created_at_unix_seconds: SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .context("system clock predates Unix epoch")?
            .as_secs(),
    };
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&barrier_path)
        .context("create one-send deployment barrier")?;
    file.write_all(&serde_json::to_vec_pretty(&barrier)?)?;
    file.write_all(b"\n")?;
    file.sync_all().context("sync deployment barrier")?;

    let returned = rpc
        .send_transaction_with_config(
            &transaction,
            RpcSendTransactionConfig {
                skip_preflight: true,
                preflight_commitment: Some(CommitmentLevel::Confirmed),
                max_retries: Some(0),
                min_context_slot: Some(simulation.context.slot),
                ..RpcSendTransactionConfig::default()
            },
        )
        .with_context(|| {
            format!("{stage} submission is ambiguous; barrier retained and blind resend forbidden")
        })?;
    if returned != signature {
        bail!("RPC returned a different {stage} signature; barrier retained and blind resend forbidden");
    }
    rpc.confirm_transaction_with_spinner(&signature, &blockhash, CommitmentConfig::finalized())
        .with_context(|| format!("confirm finalized {stage} transaction"))?;
    let finalized_slot = transaction_from_finalized(rpc, &signature, &wire, stage)?;
    Ok(StageReceiptReport {
        stage: stage.to_owned(),
        signature: signature.to_string(),
        transaction_sha256: barrier.transaction_sha256,
        message_sha256: barrier.message_sha256,
        barrier_path: barrier_path.display().to_string(),
        simulation_context_slot: barrier.simulation_context_slot,
        finalized_slot,
        reconciled_from_barrier: false,
    })
}

fn inspect_deployed_program(
    rpc: &RpcClient,
    program_id: &Pubkey,
    expected_authority: &Pubkey,
    artifact: &[u8],
    spec: &ProgramSpec,
) -> Result<Option<(VerificationReport, bool)>> {
    let program_response = rpc
        .get_account_with_commitment(program_id, CommitmentConfig::finalized())
        .context("read finalized Program account")?;
    let Some(program) = program_response.value else {
        return Ok(None);
    };
    if program.owner != upgradeable_loader_id() || !program.executable {
        bail!("pinned program id exists but is not an executable upgradeable-loader program");
    }
    if program.data.len() != UpgradeableLoaderState::size_of_program() {
        bail!("finalized Program account has an unexpected data length");
    }
    let state: UpgradeableLoaderState =
        bincode::deserialize(&program.data).context("decode finalized Program state")?;
    let expected_programdata = get_program_data_address(program_id);
    match state {
        UpgradeableLoaderState::Program {
            programdata_address,
        } if programdata_address == expected_programdata => {}
        _ => bail!("finalized Program account points to unexpected ProgramData"),
    }

    let programdata = rpc
        .get_account_with_commitment(&expected_programdata, CommitmentConfig::finalized())
        .context("read finalized ProgramData account")?
        .value
        .ok_or_else(|| anyhow!("finalized ProgramData account is missing"))?;
    if programdata.owner != upgradeable_loader_id() || programdata.executable {
        bail!("finalized ProgramData account has unexpected owner or executable flag");
    }
    if programdata.data.len() != UpgradeableLoaderState::size_of_programdata(spec.max_data_len) {
        bail!("finalized ProgramData account does not use the pinned max data length");
    }
    let metadata_len = UpgradeableLoaderState::size_of_programdata_metadata();
    let state: UpgradeableLoaderState = bincode::deserialize(&programdata.data[..metadata_len])
        .context("decode finalized ProgramData state")?;
    let (programdata_deployment_slot, upgrade_authority) = match state {
        UpgradeableLoaderState::ProgramData {
            slot,
            upgrade_authority_address: Some(authority),
            ..
        } if authority == *expected_authority => (slot, authority),
        _ => bail!("finalized ProgramData upgrade authority is not the pinned authority"),
    };
    let deployed_payload = &programdata.data[metadata_len..];
    let deployed_prefix = &deployed_payload[..artifact.len()];
    let deployed_payload_hash = hex_sha256(deployed_payload);
    let deployed_prefix_hash = hex_sha256(deployed_prefix);
    let matches = deployed_prefix == artifact
        && deployed_prefix_hash == spec.elf_sha256
        && deployed_payload[artifact.len()..]
            .iter()
            .all(|byte| *byte == 0);

    Ok(Some((
        VerificationReport {
            finalized_slot: program_response.context.slot,
            programdata_deployment_slot,
            program_executable: program.executable,
            program_owner: program.owner.to_string(),
            programdata_owner: programdata.owner.to_string(),
            programdata_bytes: programdata.data.len(),
            deployed_programdata_payload_sha256: deployed_payload_hash,
            deployed_artifact_prefix_sha256: deployed_prefix_hash,
            upgrade_authority: upgrade_authority.to_string(),
        },
        matches,
    )))
}

fn sha256_bytes(bytes: &[u8]) -> [u8; 32] {
    Sha256::digest(bytes).into()
}

fn hex_sha256(bytes: &[u8]) -> String {
    sha256_bytes(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}
