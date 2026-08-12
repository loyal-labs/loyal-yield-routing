mod kamino;
mod meteora;
mod policy;
mod returns;
mod squads;
mod state;

use anyhow::{bail, Context, Result};
use loyal_actions::{
    autonomous_vaults::{return_to_mother_instruction, TreasuryReturnKind},
    compile_squads_inner_instruction, decode_settings_signer_handoff_instruction,
    derive_kamino_user_metadata, derive_squads_v4_vault,
    execute_program_interaction_policy_instruction, execute_sync_transaction_instruction,
    handoff_settings_signer_instruction, ASSOCIATED_TOKEN_PROGRAM_ID, KAMINO_FARMS_PROGRAM_ID,
    KAMINO_LEND_PROGRAM_ID, KAMINO_MAIN_MARKET, SQUADS_V4_PROGRAM_ID, USDC_MINT,
};
use loyal_solana_env::{
    keypair_from_env, policy_keypair_from_env, rpc_safety::validate_rpc_genesis_hash,
};
use loyal_yield_store::{
    NeonSqlClient, NeonSqlConfig, PolicyMatchInput, FIXED_KAMINO_MAIN_ROUTE_MODE,
};
use solana_client::{
    rpc_client::RpcClient,
    rpc_config::{RpcSendTransactionConfig, RpcSimulateTransactionConfig, RpcTransactionConfig},
};
use solana_sdk::{
    commitment_config::{CommitmentConfig, CommitmentLevel},
    compute_budget::ComputeBudgetInstruction,
    instruction::Instruction,
    program_pack::Pack,
    pubkey::Pubkey,
    signature::{Signature, Signer},
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use state::{
    KaminoRecord, LiveStepRecord, PolicyRecord, PolicyStatus, SmartAccountRecord,
    SmartAccountStatus, VaultState,
};
use std::{
    collections::{BTreeMap, BTreeSet},
    env,
    str::FromStr,
};

const DEPLOYMENT_PK_ENV: &str = "DEPLOYMENT_PK";
const CONFIRM_MAINNET_ENV: &str = "CONFIRM_MAINNET";
const VAULT_INDEX: u8 = 0;
const SOLANA_PACKET_DATA_SIZE: u64 = 1_232;
const KAMINO_SETUP_VAULT_LAMPORTS: u64 = 100_000_000;
const KAMINO_TEST_USDC_RAW: u64 = 1_000_000;
const KFARMS_USER_STATE_BYTES: u64 = 920;
const SQUADS_EXTENDED_HEAP_FRAME_BYTES: u32 = 256_000;
const KAMINO_SINGLE_RESERVE_TEST_USDC_RAW: u64 = 100_000;
const METEORA_SETUP_VAULT_LAMPORTS: u64 = 100_000_000;
const METEORA_TEST_LOYAL_RAW: u64 = 5_000;
const METEORA_LIQUIDITY_TEST_LOYAL_RAW: u64 = 1_000;
const METEORA_LIQUIDITY_TEST_USDC_RAW: u64 = 100_000;

fn main() -> Result<()> {
    let command = env::args().nth(1).unwrap_or_else(|| "inspect".to_owned());
    let rpc_url = env::var("SOLANA_RPC_URL").context("SOLANA_RPC_URL is not set")?;
    let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::finalized());
    let deployment = keypair_from_env(DEPLOYMENT_PK_ENV).context("load DEPLOYMENT_PK")?;
    let delegated = policy_keypair_from_env().context("load POLICY_KEYPAIR")?;
    let genesis_hash = verify_mainnet(&rpc)?;
    let path = state::state_path();
    let mut persisted = state::load(&path)?;
    if let Some(state) = &persisted {
        state.validate_identity(
            &genesis_hash,
            &deployment.pubkey().to_string(),
            &delegated.pubkey().to_string(),
        )?;
    }

    match command.as_str() {
        "inspect" => inspect(&rpc, &path, persisted.as_ref(), &deployment, &delegated),
        "simulate-smart-account" => {
            simulate_new_smart_account(&rpc, persisted.as_ref(), &deployment)
        }
        "create-smart-account" => {
            require_mainnet_confirmation()?;
            let mut vault_state = persisted.take().unwrap_or_else(|| {
                VaultState::new(
                    genesis_hash,
                    deployment.pubkey().to_string(),
                    delegated.pubkey().to_string(),
                )
            });
            create_or_resume_smart_account(&rpc, &path, &mut vault_state, &deployment)?;
            inspect(&rpc, &path, Some(&vault_state), &deployment, &delegated)
        }
        "inspect-kamino" => inspect_kamino(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "inspect-meteora" => inspect_meteora(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "inspect-meteora-policy-upgrade" => inspect_meteora_policy_upgrade(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "simulate-meteora-policy-upgrade" => simulate_meteora_policy_upgrade(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "upgrade-meteora-policies" => {
            require_mainnet_confirmation()?;
            upgrade_meteora_policies(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
            )
        }
        "simulate-meteora-adversarial" => simulate_meteora_adversarial_matrix(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "inspect-returns" => inspect_returns(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "verify-all" => verify_all(
            &rpc,
            &path,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "sync-routing-control-plane" => sync_routing_control_plane(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "simulate-signer-handoff-readiness" => simulate_signer_handoff_readiness(
            &rpc,
            &path,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "simulate-return-loyal-policy" => simulate_return_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            TreasuryReturnKind::Loyal,
        ),
        "simulate-return-usdc-policy" => simulate_return_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            TreasuryReturnKind::Usdc,
        ),
        "create-return-loyal-policy" => {
            require_mainnet_confirmation()?;
            create_or_resume_return_policy(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                TreasuryReturnKind::Loyal,
            )
        }
        "create-return-usdc-policy" => {
            require_mainnet_confirmation()?;
            create_or_resume_return_policy(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                TreasuryReturnKind::Usdc,
            )
        }
        "simulate-return-loyal" => simulate_return_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            TreasuryReturnKind::Loyal,
        ),
        "simulate-return-usdc" => simulate_return_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            TreasuryReturnKind::Usdc,
        ),
        "execute-return-loyal" => {
            require_mainnet_confirmation()?;
            execute_return_to_mother(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                TreasuryReturnKind::Loyal,
            )
        }
        "execute-return-usdc" => {
            require_mainnet_confirmation()?;
            execute_return_to_mother(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                TreasuryReturnKind::Usdc,
            )
        }
        "acquire-meteora-loyal-dust" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            acquire_meteora_loyal_dust(&rpc, &path, state, &deployment, &delegated)
        }
        "setup-meteora-accounts" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            setup_meteora_accounts(&rpc, &path, state, &deployment, &delegated)
        }
        "simulate-meteora-position-expand" => simulate_meteora_position_expand(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "expand-meteora-position" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            expand_meteora_position(&rpc, &path, state, &deployment, &delegated)
        }
        "simulate-meteora-add-a" => simulate_meteora_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            MeteoraExecutionKind::AddA,
        ),
        "execute-meteora-add-a" => {
            require_mainnet_confirmation()?;
            execute_meteora_liquidity_step(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                MeteoraExecutionKind::AddA,
            )
        }
        "simulate-meteora-remove-a" => simulate_meteora_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            MeteoraExecutionKind::RemoveA,
        ),
        "execute-meteora-remove-a" => {
            require_mainnet_confirmation()?;
            execute_meteora_liquidity_step(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                MeteoraExecutionKind::RemoveA,
            )
        }
        "simulate-meteora-add-b" => simulate_meteora_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            MeteoraExecutionKind::AddB,
        ),
        "execute-meteora-add-b" => {
            require_mainnet_confirmation()?;
            execute_meteora_liquidity_step(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                MeteoraExecutionKind::AddB,
            )
        }
        "simulate-meteora-fee-swap" => simulate_meteora_fee_swap(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "generate-meteora-fees" => {
            require_mainnet_confirmation()?;
            execute_meteora_fee_swap(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
            )
        }
        "simulate-meteora-remove-b" => simulate_meteora_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            MeteoraExecutionKind::RemoveB,
        ),
        "execute-meteora-remove-b" => {
            require_mainnet_confirmation()?;
            execute_meteora_liquidity_step(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
                MeteoraExecutionKind::RemoveB,
            )
        }
        "simulate-meteora-claim-fees" => simulate_meteora_claim_fees(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "claim-meteora-fees" => {
            require_mainnet_confirmation()?;
            execute_meteora_claim_fees(
                &rpc,
                &path,
                persisted.as_mut().context("Smart Account state is missing")?,
                &deployment,
                &delegated,
            )
        }
        "simulate-meteora-add-policy" => simulate_meteora_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            meteora::MeteoraPolicyKind::AddLiquidity,
        ),
        "simulate-meteora-remove-policy" => simulate_meteora_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            meteora::MeteoraPolicyKind::RemoveLiquidity,
        ),
        "simulate-meteora-claim-policy" => simulate_meteora_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            meteora::MeteoraPolicyKind::ClaimFees,
        ),
        "create-meteora-add-policy" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            create_or_resume_meteora_policy(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                meteora::MeteoraPolicyKind::AddLiquidity,
            )
        }
        "create-meteora-remove-policy" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            create_or_resume_meteora_policy(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                meteora::MeteoraPolicyKind::RemoveLiquidity,
            )
        }
        "create-meteora-claim-policy" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            create_or_resume_meteora_policy(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                meteora::MeteoraPolicyKind::ClaimFees,
            )
        }
        "simulate-kamino-operations-policy" => simulate_kamino_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            KaminoPolicyKind::Operations,
        ),
        "simulate-kamino-init-policy" => simulate_kamino_policy(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
            KaminoPolicyKind::InitObligation,
        ),
        "create-kamino-operations-policy" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            create_or_resume_kamino_policy(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                KaminoPolicyKind::Operations,
            )
        }
        "create-kamino-init-policy" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            create_or_resume_kamino_policy(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                KaminoPolicyKind::InitObligation,
            )
        }
        "inspect-kamino-execution" => inspect_kamino_execution(
            &rpc,
            persisted.as_ref().context("Smart Account state is missing")?,
            &deployment,
            &delegated,
        ),
        "setup-kamino-accounts" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            setup_kamino_accounts(&rpc, &path, state, &deployment, &delegated)
        }
        "init-kamino-obligation" => {
            require_mainnet_confirmation()?;
            let reserve_index = parse_reserve_index()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            init_kamino_obligation(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                reserve_index,
            )
        }
        "reinit-kamino-obligation" => {
            require_mainnet_confirmation()?;
            let reserve_index = parse_reserve_index()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            reinit_kamino_obligation(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                reserve_index,
            )
        }
        "setup-kamino-farms" => {
            require_mainnet_confirmation()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            setup_kamino_farms(&rpc, &path, state, &deployment, &delegated)
        }
        "deposit-kamino" => {
            require_mainnet_confirmation()?;
            let reserve_index = parse_reserve_index()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            execute_kamino_operation(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                reserve_index,
                KaminoOperationKind::Deposit,
            )
        }
        "withdraw-kamino-partial" => {
            require_mainnet_confirmation()?;
            let reserve_index = parse_reserve_index()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            execute_kamino_operation(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                reserve_index,
                KaminoOperationKind::PartialWithdraw,
            )
        }
        "withdraw-kamino-full" => {
            require_mainnet_confirmation()?;
            let reserve_index = parse_reserve_index()?;
            let state = persisted.as_mut().context("Smart Account state is missing")?;
            execute_kamino_operation(
                &rpc,
                &path,
                state,
                &deployment,
                &delegated,
                reserve_index,
                KaminoOperationKind::FullWithdraw,
            )
        }
        _ => bail!(
            "unknown command {command:?}; expected inspect, Smart Account commands, or Kamino policy commands"
        ),
    }
}

fn simulate_new_smart_account(
    rpc: &RpcClient,
    persisted: Option<&VaultState>,
    deployment: &solana_sdk::signature::Keypair,
) -> Result<()> {
    if persisted
        .and_then(|state| state.smart_account.as_ref())
        .is_some()
    {
        bail!("state already contains a Smart Account plan; use inspect or resume creation");
    }
    let program_config_address = squads::derive_program_config();
    let config_account = rpc
        .get_account(&program_config_address)
        .context("fetch Squads ProgramConfig before simulation")?;
    let config = squads::decode_program_config(&config_account.data)?;
    let account_index = config
        .smart_account_index
        .checked_add(1)
        .context("Squads account index overflow")?;
    let settings = squads::derive_settings(account_index);
    if rpc.get_account(&settings).is_ok() {
        bail!("next derived Settings account already exists");
    }
    let vault = squads::derive_vault(settings, VAULT_INDEX);
    let instruction =
        squads::create_smart_account_instruction(deployment.pubkey(), config.treasury, settings)?;
    let blockhash = rpc
        .get_latest_blockhash_with_commitment(CommitmentConfig::finalized())
        .context("fetch finalized blockhash for simulation")?
        .0;
    let transaction = Transaction::new_signed_with_payer(
        &[instruction],
        Some(&deployment.pubkey()),
        &[deployment],
        blockhash,
    );
    let simulation = rpc
        .simulate_transaction_with_config(
            &transaction,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                replace_recent_blockhash: false,
                commitment: Some(CommitmentConfig::finalized()),
                ..RpcSimulateTransactionConfig::default()
            },
        )
        .context("simulate createSmartAccount transaction")?;
    if let Some(error) = simulation.value.err {
        bail!("createSmartAccount simulation failed: {error:?}");
    }
    println!("module=smart-account-simulation verdict=PASS");
    println!("account_index={account_index}");
    println!("settings={settings}");
    println!("vault_index={VAULT_INDEX}");
    println!("vault={vault}");
    println!("deployment_signer={}", deployment.pubkey());
    println!(
        "units_consumed={}",
        simulation
            .value
            .units_consumed
            .map(|value| value.to_string())
            .as_deref()
            .unwrap_or("unknown")
    );
    println!("transaction_sent=false");
    Ok(())
}

fn verify_mainnet(rpc: &RpcClient) -> Result<String> {
    let observed = rpc.get_genesis_hash().context("fetch RPC genesis hash")?;
    validate_rpc_genesis_hash("mainnet-beta", observed)
        .map_err(anyhow::Error::msg)
        .context("verify mainnet-beta RPC")?;
    Ok(observed.to_string())
}

fn require_mainnet_confirmation() -> Result<()> {
    if env::var(CONFIRM_MAINNET_ENV).as_deref() != Ok("1") {
        bail!("mutating mainnet commands require CONFIRM_MAINNET=1");
    }
    Ok(())
}

fn inspect(
    rpc: &RpcClient,
    path: &std::path::Path,
    persisted: Option<&VaultState>,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let program_config_address = squads::derive_program_config();
    let config_account = rpc
        .get_account(&program_config_address)
        .context("fetch Squads ProgramConfig")?;
    if config_account.owner != loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID {
        bail!("Squads ProgramConfig has an unexpected owner");
    }
    let config = squads::decode_program_config(&config_account.data)?;

    println!("module=smart-account-readiness verdict=PASS");
    println!("cluster=mainnet-beta");
    println!("genesis_hash={}", rpc.get_genesis_hash()?);
    println!("program_config={program_config_address}");
    println!("program_config_index={}", config.smart_account_index);
    println!("program_treasury={}", config.treasury);
    println!(
        "smart_account_creation_fee_lamports={}",
        config.smart_account_creation_fee
    );
    println!("deployment_signer={}", deployment.pubkey());
    println!("delegated_policy_signer={}", delegated.pubkey());
    println!(
        "deployment_balance_lamports={}",
        rpc.get_balance(&deployment.pubkey())?
    );
    println!("state_file={}", path.display());

    let Some(state) = persisted else {
        println!("created_smart_account=PENDING");
        return Ok(());
    };
    let Some(record) = &state.smart_account else {
        println!("created_smart_account=PENDING");
        return Ok(());
    };
    println!("smart_account_status={:?}", record.status);
    println!("settings={}", record.settings);
    println!("vault_index={}", record.vault_index);
    println!("vault={}", record.vault);
    if record.status == SmartAccountStatus::Finalized {
        verify_recorded_settings(rpc, record, deployment.pubkey())?;
        println!("created_smart_account=PASS");
        println!(
            "creation_signature={}",
            record.creation_signature.as_deref().unwrap_or("MISSING")
        );
        println!(
            "finalized_slot={}",
            record
                .finalized_slot
                .map(|slot| slot.to_string())
                .as_deref()
                .unwrap_or("MISSING")
        );
    }
    Ok(())
}

fn create_or_resume_smart_account(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
) -> Result<()> {
    if let Some(record) = &state.smart_account {
        return match record.status {
            SmartAccountStatus::Finalized => {
                verify_recorded_settings(rpc, record, deployment.pubkey())?;
                println!("smart-account create is already finalized; refusing to create another");
                Ok(())
            }
            SmartAccountStatus::Planned => resume_planned_creation(rpc, path, state, deployment),
        };
    }

    let program_config_address = squads::derive_program_config();
    let config_account = rpc
        .get_account(&program_config_address)
        .context("fetch Squads ProgramConfig before create")?;
    let config = squads::decode_program_config(&config_account.data)?;
    let account_index = config
        .smart_account_index
        .checked_add(1)
        .context("Squads account index overflow")?;
    let settings = squads::derive_settings(account_index);
    if rpc.get_account(&settings).is_ok() {
        bail!("next derived Settings account already exists; refusing ambiguous creation");
    }
    let vault = squads::derive_vault(settings, VAULT_INDEX);
    state.smart_account = Some(SmartAccountRecord {
        status: SmartAccountStatus::Planned,
        account_index: account_index.to_string(),
        settings: settings.to_string(),
        vault_index: VAULT_INDEX,
        vault: vault.to_string(),
        program_config: program_config_address.to_string(),
        program_treasury: config.treasury.to_string(),
        pending_signature: None,
        last_valid_block_height: None,
        creation_signature: None,
        finalized_slot: None,
    });
    state::save(path, state)?;
    send_planned_creation(rpc, path, state, deployment)
}

fn resume_planned_creation(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let record = state
        .smart_account
        .as_ref()
        .context("planned Smart Account record disappeared")?;
    if let Some(signature) = record.pending_signature.as_deref() {
        let signature = Signature::from_str(signature).context("parse pending signature")?;
        let statuses = rpc
            .get_signature_statuses(&[signature])
            .context("fetch pending signature status")?;
        if let Some(status) = statuses.value.into_iter().next().flatten() {
            if let Some(error) = status.err {
                bail!("recorded Smart Account creation failed on chain: {error:?}");
            }
            if status.satisfies_commitment(CommitmentConfig::finalized()) {
                return finalize_record(rpc, path, state, deployment.pubkey(), signature);
            }
            bail!("recorded Smart Account creation is not finalized yet");
        }
        let last_valid = record
            .last_valid_block_height
            .context("planned creation is missing last valid block height")?;
        let current = rpc
            .get_block_height()
            .context("fetch current block height")?;
        if current <= last_valid {
            bail!("recorded creation signature is still live but not visible; retry later");
        }
    }

    let expected_index = record
        .account_index
        .parse::<u128>()
        .context("parse planned account index")?;
    let settings = solana_sdk::pubkey::Pubkey::from_str(&record.settings)
        .context("parse planned Settings address")?;
    if rpc.get_account(&settings).is_ok() {
        bail!("planned Settings account exists but its creation signature was not recovered");
    }
    let config_account = rpc.get_account(&squads::derive_program_config())?;
    let config = squads::decode_program_config(&config_account.data)?;
    if config.smart_account_index.checked_add(1) != Some(expected_index) {
        bail!("Squads global index advanced after planning; refusing to create a second account");
    }
    send_planned_creation(rpc, path, state, deployment)
}

fn send_planned_creation(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let record = state
        .smart_account
        .as_ref()
        .context("missing planned Smart Account")?;
    let settings = solana_sdk::pubkey::Pubkey::from_str(&record.settings)?;
    let treasury = solana_sdk::pubkey::Pubkey::from_str(&record.program_treasury)?;
    let instruction =
        squads::create_smart_account_instruction(deployment.pubkey(), treasury, settings)?;
    let (blockhash, last_valid_block_height) = rpc
        .get_latest_blockhash_with_commitment(CommitmentConfig::finalized())
        .context("fetch finalized blockhash")?;
    let transaction = Transaction::new_signed_with_payer(
        &[instruction],
        Some(&deployment.pubkey()),
        &[deployment],
        blockhash,
    );

    let simulation = rpc
        .simulate_transaction_with_config(
            &transaction,
            RpcSimulateTransactionConfig {
                sig_verify: true,
                replace_recent_blockhash: false,
                commitment: Some(CommitmentConfig::finalized()),
                ..RpcSimulateTransactionConfig::default()
            },
        )
        .context("simulate createSmartAccount transaction")?;
    if let Some(error) = simulation.value.err {
        bail!("createSmartAccount simulation failed: {error:?}");
    }
    println!(
        "create_smart_account_simulation=PASS units_consumed={}",
        simulation
            .value
            .units_consumed
            .map(|value| value.to_string())
            .as_deref()
            .unwrap_or("unknown")
    );

    let pending_signature = transaction.signatures[0];
    let record = state
        .smart_account
        .as_mut()
        .context("missing planned Smart Account")?;
    record.pending_signature = Some(pending_signature.to_string());
    record.last_valid_block_height = Some(last_valid_block_height);
    state::save(path, state)?;

    let sent_signature = rpc
        .send_transaction_with_config(
            &transaction,
            RpcSendTransactionConfig {
                skip_preflight: false,
                preflight_commitment: Some(CommitmentLevel::Finalized),
                ..RpcSendTransactionConfig::default()
            },
        )
        .context("send createSmartAccount transaction")?;
    if sent_signature != pending_signature {
        bail!("RPC returned a different transaction signature than the signed transaction");
    }
    rpc.confirm_transaction_with_spinner(
        &sent_signature,
        &blockhash,
        CommitmentConfig::finalized(),
    )
    .context("confirm createSmartAccount transaction")?;
    finalize_record(rpc, path, state, deployment.pubkey(), sent_signature)
}

fn finalize_record(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: solana_sdk::pubkey::Pubkey,
    signature: Signature,
) -> Result<()> {
    let record = state
        .smart_account
        .as_ref()
        .context("missing Smart Account record")?;
    verify_recorded_settings(rpc, record, deployment)?;
    let transaction = rpc
        .get_transaction_with_config(
            &signature,
            RpcTransactionConfig {
                encoding: None,
                commitment: Some(CommitmentConfig::finalized()),
                max_supported_transaction_version: Some(0),
            },
        )
        .context("fetch finalized Smart Account creation transaction")?;
    let record = state
        .smart_account
        .as_mut()
        .context("missing Smart Account record")?;
    record.status = SmartAccountStatus::Finalized;
    record.creation_signature = Some(signature.to_string());
    record.finalized_slot = Some(transaction.slot);
    state::save(path, state)?;
    println!(
        "create_smart_account=PASS signature={signature} slot={}",
        transaction.slot
    );
    Ok(())
}

fn verify_recorded_settings(
    rpc: &RpcClient,
    record: &SmartAccountRecord,
    deployment: solana_sdk::pubkey::Pubkey,
) -> Result<()> {
    let settings = solana_sdk::pubkey::Pubkey::from_str(&record.settings)?;
    let vault = solana_sdk::pubkey::Pubkey::from_str(&record.vault)?;
    let account_index = record.account_index.parse::<u128>()?;
    if squads::derive_settings(account_index) != settings
        || squads::derive_vault(settings, record.vault_index) != vault
    {
        bail!("recorded Settings or vault address does not match independent PDA derivation");
    }
    let account = rpc
        .get_account(&settings)
        .context("reload recorded Settings account")?;
    squads::verify_created_settings(account.owner, &account.data, account_index, deployment)?;
    Ok(())
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum KaminoPolicyKind {
    Operations,
    InitObligation,
}

impl KaminoPolicyKind {
    fn seed(self) -> u64 {
        match self {
            Self::Operations => kamino::KAMINO_OPERATIONS_POLICY_SEED,
            Self::InitObligation => kamino::KAMINO_INIT_POLICY_SEED,
        }
    }

    fn label(self) -> &'static str {
        match self {
            Self::Operations => "kamino-operations",
            Self::InitObligation => "kamino-init-obligation",
        }
    }
}

fn load_kamino_plan(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<(Pubkey, Pubkey, kamino::KaminoPlan)> {
    let smart_account = state
        .smart_account
        .as_ref()
        .context("Smart Account record is missing")?;
    if smart_account.status != SmartAccountStatus::Finalized {
        bail!("Smart Account must be finalized before planning Kamino policies");
    }
    verify_recorded_settings(rpc, smart_account, deployment.pubkey())?;
    let settings = Pubkey::from_str(&smart_account.settings)?;
    let vault = Pubkey::from_str(&smart_account.vault)?;
    let plan = kamino::load_plan(
        rpc,
        settings,
        deployment.pubkey(),
        delegated.pubkey(),
        vault,
        smart_account.vault_index,
    )?;
    if let Some(record) = &state.kamino {
        kamino::validate_record(record, &plan)?;
    }
    Ok((settings, vault, plan))
}

fn load_meteora_plan(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<(Pubkey, Pubkey, meteora::MeteoraPlan)> {
    let generation = state
        .meteora
        .as_ref()
        .map(|record| record.policy_generation)
        .unwrap_or(meteora::METEORA_LEGACY_POLICY_GENERATION);
    load_meteora_plan_for_generation(rpc, state, deployment, delegated, generation, true, true)
}

fn load_meteora_plan_for_generation(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    generation: u8,
    validate_record: bool,
    require_bin_arrays: bool,
) -> Result<(Pubkey, Pubkey, meteora::MeteoraPlan)> {
    let smart_account = state
        .smart_account
        .as_ref()
        .context("Smart Account record is missing")?;
    if smart_account.status != SmartAccountStatus::Finalized {
        bail!("Smart Account must be finalized before planning Meteora policies");
    }
    verify_recorded_settings(rpc, smart_account, deployment.pubkey())?;
    let settings = Pubkey::from_str(&smart_account.settings)?;
    let vault = Pubkey::from_str(&smart_account.vault)?;
    let plan = meteora::load_plan(
        rpc,
        settings,
        deployment.pubkey(),
        delegated.pubkey(),
        vault,
        smart_account.vault_index,
        generation,
        require_bin_arrays,
    )?;
    if validate_record {
        let record = state
            .meteora
            .as_ref()
            .context("Meteora record is missing")?;
        meteora::validate_record(record, &plan)?;
    }
    Ok((settings, vault, plan))
}

fn inspect_meteora(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (settings, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    let deployment_loyal = derive_associated_token_address(
        deployment.pubkey(),
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    );
    let deployment_usdc = derive_associated_token_address(deployment.pubkey(), USDC_MINT);
    let position = meteora::load_position_snapshot(rpc, plan.position, vault)?;
    let settings_account = rpc.get_account(&settings)?;
    let decoded_settings = squads::decode_settings(&settings_account.data)?;

    println!("module=meteora-readiness verdict=PASS");
    println!("policy_generation={}", plan.policy_generation);
    println!("settings={settings}");
    println!("vault={vault}");
    println!("pool={}", loyal_actions::autonomous_vaults::METEORA_POOL);
    println!("source_slot={}", plan.source_slot);
    println!("active_bin_id={}", plan.active_bin_id);
    println!(
        "position={} lower_bin_id={} upper_bin_id={} width={}",
        plan.position, plan.position_lower_bin_id, plan.position_upper_bin_id, plan.position_width
    );
    println!(
        "strategy_range_a={}..={} strategy_range_b={}..={}",
        plan.range_a.min, plan.range_a.max, plan.range_b.min, plan.range_b.max
    );
    println!(
        "position_exists={} position_lamports={} position_data_len={} nonzero_liquidity_bins={} pending_fee_loyal_raw={} pending_fee_usdc_raw={}",
        position.is_some(),
        position.map(|snapshot| snapshot.lamports).unwrap_or(0),
        position.map(|snapshot| snapshot.data_len).unwrap_or(0),
        position
            .map(|snapshot| snapshot.nonzero_liquidity_bins)
            .unwrap_or(0),
        position.map(|snapshot| snapshot.pending_fee_x).unwrap_or(0),
        position.map(|snapshot| snapshot.pending_fee_y).unwrap_or(0)
    );
    println!("vault_lamports={}", rpc.get_balance(&vault)?);
    println!("deployment_loyal_token_account={deployment_loyal}");
    println!(
        "deployment_loyal_raw={}",
        token_account_amount(
            rpc,
            deployment_loyal,
            deployment.pubkey(),
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .unwrap_or(0)
    );
    println!("deployment_usdc_token_account={deployment_usdc}");
    println!(
        "deployment_usdc_raw={}",
        token_account_amount(rpc, deployment_usdc, deployment.pubkey(), USDC_MINT)?.unwrap_or(0)
    );
    println!("vault_loyal_token_account={}", plan.vault_loyal);
    println!(
        "vault_loyal_raw={}",
        token_account_amount(
            rpc,
            plan.vault_loyal,
            vault,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .unwrap_or(0)
    );
    println!("vault_usdc_token_account={}", plan.vault_usdc);
    println!(
        "vault_usdc_raw={}",
        token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?.unwrap_or(0)
    );
    println!(
        "pool_loyal_reserve_raw={}",
        token_account_amount(
            rpc,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_RESERVE,
            loyal_actions::autonomous_vaults::METEORA_POOL,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .context("Meteora LOYAL reserve is absent")?
    );
    println!(
        "pool_usdc_reserve_raw={}",
        token_account_amount(
            rpc,
            loyal_actions::autonomous_vaults::METEORA_USDC_RESERVE,
            loyal_actions::autonomous_vaults::METEORA_POOL,
            USDC_MINT,
        )?
        .context("Meteora USDC reserve is absent")?
    );
    println!(
        "bitmap_extension_or_program_sentinel={}",
        plan.bitmap_extension_or_program_sentinel
    );
    for (index, bin_array) in plan.bin_arrays.iter().enumerate() {
        println!("approved_bin_array_slot={index} address={bin_array}");
    }
    println!(
        "settings_policy_seed={}",
        decoded_settings
            .policy_seed
            .map(|seed| seed.to_string())
            .as_deref()
            .unwrap_or("none")
    );
    for kind in [
        meteora::MeteoraPolicyKind::AddLiquidity,
        meteora::MeteoraPolicyKind::RemoveLiquidity,
        meteora::MeteoraPolicyKind::ClaimFees,
    ] {
        let policy_plan = meteora_policy_plan(&plan, kind).0;
        let status = state
            .meteora
            .as_ref()
            .and_then(|record| meteora::policy_record(record, kind));
        println!(
            "policy={} status={} address={} seed={}",
            kind.label(),
            status
                .map(|record| format!("{:?}", record.status))
                .as_deref()
                .unwrap_or("PENDING"),
            policy_plan.policy,
            policy_plan.policy_seed
        );
    }
    if let Some(record) = state.meteora.as_ref() {
        for shard in &plan.additional_policy_shards {
            let recorded_shard = record
                .additional_policy_shards
                .iter()
                .find(|candidate| candidate.shard_index == shard.shard_index);
            for kind in [
                meteora::MeteoraPolicyKind::AddLiquidity,
                meteora::MeteoraPolicyKind::RemoveLiquidity,
                meteora::MeteoraPolicyKind::ClaimFees,
            ] {
                let policy_plan = meteora_shard_policy_plan(shard, kind).0;
                let status = recorded_shard.and_then(|recorded| match kind {
                    meteora::MeteoraPolicyKind::AddLiquidity => {
                        recorded.add_liquidity_policy.as_ref()
                    }
                    meteora::MeteoraPolicyKind::RemoveLiquidity => {
                        recorded.remove_liquidity_policy.as_ref()
                    }
                    meteora::MeteoraPolicyKind::ClaimFees => recorded.claim_fee_policy.as_ref(),
                });
                println!(
                    "policy_shard={} policy={} status={} address={} seed={} lower_bin_array_indexes={:?}",
                    shard.shard_index,
                    kind.label(),
                    status
                        .map(|policy| format!("{:?}", policy.status))
                        .as_deref()
                        .unwrap_or("PENDING"),
                    policy_plan.policy,
                    policy_plan.policy_seed,
                    shard.lower_bin_array_indexes
                );
            }
        }
    }
    Ok(())
}

fn meteora_policy_plan(
    plan: &meteora::MeteoraPlan,
    kind: meteora::MeteoraPolicyKind,
) -> (
    &loyal_actions::autonomous_vaults::MeteoraPolicyPlan,
    &[loyal_actions::SquadsInstructionConstraintView],
) {
    match kind {
        meteora::MeteoraPolicyKind::AddLiquidity => {
            (&plan.policies.add_liquidity, &plan.add_constraints)
        }
        meteora::MeteoraPolicyKind::RemoveLiquidity => {
            (&plan.policies.remove_liquidity, &plan.remove_constraints)
        }
        meteora::MeteoraPolicyKind::ClaimFees => {
            (&plan.policies.claim_fees, &plan.claim_constraints)
        }
    }
}

fn meteora_shard_policy_plan(
    shard: &meteora::MeteoraPolicyShardPlan,
    kind: meteora::MeteoraPolicyKind,
) -> (
    &loyal_actions::autonomous_vaults::MeteoraPolicyPlan,
    &[loyal_actions::SquadsInstructionConstraintView],
) {
    match kind {
        meteora::MeteoraPolicyKind::AddLiquidity => {
            (&shard.policies.add_liquidity, &shard.add_constraints)
        }
        meteora::MeteoraPolicyKind::RemoveLiquidity => {
            (&shard.policies.remove_liquidity, &shard.remove_constraints)
        }
        meteora::MeteoraPolicyKind::ClaimFees => {
            (&shard.policies.claim_fees, &shard.claim_constraints)
        }
    }
}

fn inspect_meteora_policy_upgrade(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (settings, vault, current) = load_meteora_plan(rpc, state, deployment, delegated)?;
    if current.policy_generation != meteora::METEORA_LEGACY_POLICY_GENERATION {
        println!(
            "module=meteora-policy-upgrade-readiness verdict=PASS already_upgraded=true generation={}",
            current.policy_generation
        );
        return Ok(());
    }
    let (_, _, expanded) = load_meteora_plan_for_generation(
        rpc,
        state,
        deployment,
        delegated,
        meteora::METEORA_EXPANDED_POLICY_GENERATION,
        false,
        true,
    )?;
    if expanded.position != current.position
        || expanded.position_lower_bin_id != meteora::POSITION_LOWER_BIN_ID
        || expanded.position_upper_bin_id != meteora::POSITION_TARGET_UPPER_BIN_ID
    {
        bail!("expanded Meteora policy plan changed the approved position identity or bounds");
    }
    verify_next_policy_seed(rpc, settings, meteora::METEORA_EXPANDED_ADD_POLICY_SEED)?;

    println!("module=meteora-policy-upgrade-readiness verdict=PASS");
    println!("settings={settings}");
    println!("vault={vault}");
    println!("position={}", expanded.position);
    println!(
        "physical_bounds={}..={} policy_generation_before={} policy_generation_after={}",
        expanded.position_lower_bin_id,
        expanded.position_upper_bin_id,
        current.policy_generation,
        expanded.policy_generation
    );
    for ((index, address), planned_address) in meteora::expanded_bin_array_candidates()
        .into_iter()
        .zip(expanded.bin_arrays.iter())
    {
        if address != *planned_address {
            bail!("expanded Meteora BinArray derivation order changed");
        }
        let account = rpc
            .get_account_with_commitment(&address, CommitmentConfig::finalized())?
            .value;
        println!(
            "bin_array_index={index} address={address} exists={} lamports={} data_len={}",
            account.is_some(),
            account.as_ref().map(|value| value.lamports).unwrap_or(0),
            account.as_ref().map(|value| value.data.len()).unwrap_or(0)
        );
    }
    for shard in &expanded.additional_policy_shards {
        for kind in [
            meteora::MeteoraPolicyKind::AddLiquidity,
            meteora::MeteoraPolicyKind::RemoveLiquidity,
            meteora::MeteoraPolicyKind::ClaimFees,
        ] {
            let policy_plan = meteora_shard_policy_plan(shard, kind).0;
            let (transaction, _, _) =
                build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
            let packet_bytes = bincode::serialized_size(&transaction)?;
            if packet_bytes > SOLANA_PACKET_DATA_SIZE {
                bail!(
                    "Meteora policy shard {} {} is {} bytes and exceeds the Solana packet limit",
                    shard.shard_index,
                    kind.label(),
                    packet_bytes
                );
            }
            println!(
                "expanded_policy_shard={} policy={} seed={} address={} packet_bytes={}",
                shard.shard_index,
                kind.label(),
                policy_plan.policy_seed,
                policy_plan.policy,
                packet_bytes
            );
        }
    }
    println!("transaction_sent=false");
    Ok(())
}

fn simulate_meteora_policy_upgrade(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    inspect_meteora_policy_upgrade(rpc, state, deployment, delegated)?;
    let current_generation = state
        .meteora
        .as_ref()
        .context("Meteora state is missing")?
        .policy_generation;
    if current_generation == meteora::METEORA_EXPANDED_POLICY_GENERATION {
        println!("meteora_policy_upgrade_simulation=PASS already_upgraded=true");
        return Ok(());
    }
    let (settings, _, expanded) = load_meteora_plan_for_generation(
        rpc,
        state,
        deployment,
        delegated,
        meteora::METEORA_EXPANDED_POLICY_GENERATION,
        false,
        true,
    )?;
    let first_shard = expanded
        .additional_policy_shards
        .first()
        .context("expanded Meteora plan has no additional policy shards")?;
    let policy_plan =
        meteora_shard_policy_plan(first_shard, meteora::MeteoraPolicyKind::AddLiquidity).0;
    verify_next_policy_seed(rpc, settings, policy_plan.policy_seed)?;
    let (transaction, _, _) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, "meteora-policy-upgrade-first")?;
    println!("meteora_policy_upgrade_simulation=PASS units_consumed={units}");
    println!("first_policy={}", policy_plan.policy);
    println!("first_policy_seed={}", policy_plan.policy_seed);
    println!("transaction_sent=false");
    Ok(())
}

fn meteora_upgrade_step_name(shard_index: u8, kind: meteora::MeteoraPolicyKind) -> String {
    format!(
        "meteora-policy-upgrade-shard-{shard_index}-{}",
        match kind {
            meteora::MeteoraPolicyKind::AddLiquidity => "add",
            meteora::MeteoraPolicyKind::RemoveLiquidity => "remove",
            meteora::MeteoraPolicyKind::ClaimFees => "claim",
        }
    )
}

fn meteora_policy_upgrade_observations(
    rpc: &RpcClient,
    settings: Pubkey,
    policy_address: Pubkey,
) -> Result<BTreeMap<String, u64>> {
    let settings_account = rpc.get_account(&settings)?;
    let decoded = squads::decode_settings(&settings_account.data)?;
    let mut observations = BTreeMap::new();
    observations.insert(
        "settings_policy_seed".to_owned(),
        decoded.policy_seed.unwrap_or(0),
    );
    observations.insert(
        "policy_exists".to_owned(),
        u64::from(
            rpc.get_account_with_commitment(&policy_address, CommitmentConfig::finalized())?
                .value
                .is_some(),
        ),
    );
    Ok(observations)
}

#[allow(clippy::too_many_arguments)]
fn create_or_resume_meteora_upgrade_policy(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    settings: Pubkey,
    shard: &meteora::MeteoraPolicyShardPlan,
    kind: meteora::MeteoraPolicyKind,
) -> Result<()> {
    let (policy_plan, constraints) = meteora_shard_policy_plan(shard, kind);
    let step_name = meteora_upgrade_step_name(shard.shard_index, kind);
    let before = meteora_policy_upgrade_observations(rpc, settings, policy_plan.policy)?;
    ensure_meteora_live_step(path, state, &step_name, before)?;

    if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, &step_name)? {
        verify_meteora_policy_plan_account(
            rpc,
            policy_plan,
            constraints,
            settings,
            deployment.pubkey(),
            delegated.pubkey(),
            &step_name,
        )?;
        let after = meteora_policy_upgrade_observations(rpc, settings, policy_plan.policy)?;
        if after.get("settings_policy_seed") != Some(&policy_plan.policy_seed)
            || after.get("policy_exists") != Some(&1)
        {
            bail!("{step_name} recovered state does not match the exact policy manifest");
        }
        return finalize_meteora_live_step(rpc, path, state, &step_name, signature, after);
    }
    if meteora_live_step(state, &step_name)?.status == PolicyStatus::Finalized {
        verify_meteora_policy_plan_account(
            rpc,
            policy_plan,
            constraints,
            settings,
            deployment.pubkey(),
            delegated.pubkey(),
            &step_name,
        )?;
        println!("{step_name}=PASS already_finalized=true");
        return Ok(());
    }
    let recorded_before = &meteora_live_step(state, &step_name)?.before;
    if recorded_before.get("policy_exists") != Some(&0)
        || recorded_before.get("settings_policy_seed")
            != Some(&policy_plan.policy_seed.saturating_sub(1))
    {
        bail!("{step_name} recorded prerequisites do not match the expected policy sequence");
    }
    if rpc
        .get_account_with_commitment(&policy_plan.policy, CommitmentConfig::finalized())?
        .value
        .is_some()
    {
        bail!("{step_name} policy exists without recoverable finalized evidence");
    }
    verify_next_policy_seed(rpc, settings, policy_plan.policy_seed)?;
    let (transaction, blockhash, last_valid_block_height) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let packet_bytes = bincode::serialized_size(&transaction)?;
    if packet_bytes > SOLANA_PACKET_DATA_SIZE {
        bail!("{step_name} exceeds the Solana packet limit at {packet_bytes} bytes");
    }
    let units = simulate_signed_transaction(rpc, &transaction, &step_name)?;
    println!("{step_name}_simulation=PASS units_consumed={units} packet_bytes={packet_bytes}");
    let signature = send_meteora_live_transaction(
        rpc,
        path,
        state,
        &step_name,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    verify_meteora_policy_plan_account(
        rpc,
        policy_plan,
        constraints,
        settings,
        deployment.pubkey(),
        delegated.pubkey(),
        &step_name,
    )?;
    let after = meteora_policy_upgrade_observations(rpc, settings, policy_plan.policy)?;
    if after.get("settings_policy_seed") != Some(&policy_plan.policy_seed)
        || after.get("policy_exists") != Some(&1)
    {
        bail!("{step_name} finalized state does not match the exact policy manifest");
    }
    finalize_meteora_live_step(rpc, path, state, &step_name, signature, after)
}

fn policy_record_from_meteora_upgrade_step(
    state: &VaultState,
    shard: &meteora::MeteoraPolicyShardPlan,
    kind: meteora::MeteoraPolicyKind,
) -> Result<PolicyRecord> {
    let policy_plan = meteora_shard_policy_plan(shard, kind).0;
    let step_name = meteora_upgrade_step_name(shard.shard_index, kind);
    let step = meteora_live_step(state, &step_name)?;
    if step.status != PolicyStatus::Finalized {
        bail!("{step_name} is not finalized");
    }
    Ok(PolicyRecord {
        status: PolicyStatus::Finalized,
        seed: policy_plan.policy_seed,
        policy: policy_plan.policy.to_string(),
        pending_signature: step.pending_signature.clone(),
        last_valid_block_height: step.last_valid_block_height,
        creation_signature: step.finalized_signature.clone(),
        finalized_slot: step.finalized_slot,
    })
}

fn upgrade_meteora_policies(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    require_finalized_meteora_policies(state)?;
    let current_generation = state
        .meteora
        .as_ref()
        .context("Meteora state is missing")?
        .policy_generation;
    let (settings, _, expanded) = load_meteora_plan_for_generation(
        rpc,
        state,
        deployment,
        delegated,
        meteora::METEORA_EXPANDED_POLICY_GENERATION,
        current_generation == meteora::METEORA_EXPANDED_POLICY_GENERATION,
        true,
    )?;
    if current_generation == meteora::METEORA_EXPANDED_POLICY_GENERATION {
        verify_all_meteora_policy_accounts(
            rpc,
            &expanded,
            settings,
            deployment.pubkey(),
            delegated.pubkey(),
        )?;
        println!("meteora_policy_upgrade=PASS already_finalized=true");
        return Ok(());
    }
    if current_generation != meteora::METEORA_LEGACY_POLICY_GENERATION {
        bail!("unsupported current Meteora policy generation {current_generation}");
    }
    if expanded.position_upper_bin_id != meteora::POSITION_TARGET_UPPER_BIN_ID {
        bail!("Meteora position must be expanded before policy authorization");
    }

    for shard in &expanded.additional_policy_shards {
        for kind in [
            meteora::MeteoraPolicyKind::AddLiquidity,
            meteora::MeteoraPolicyKind::RemoveLiquidity,
            meteora::MeteoraPolicyKind::ClaimFees,
        ] {
            create_or_resume_meteora_upgrade_policy(
                rpc, path, state, deployment, delegated, settings, shard, kind,
            )?;
        }
    }

    let additional_policy_shards = expanded
        .additional_policy_shards
        .iter()
        .map(|shard| {
            Ok(state::MeteoraPolicyShardRecord {
                shard_index: shard.shard_index,
                lower_bin_array_indexes: shard.lower_bin_array_indexes.clone(),
                add_liquidity_policy: Some(policy_record_from_meteora_upgrade_step(
                    state,
                    shard,
                    meteora::MeteoraPolicyKind::AddLiquidity,
                )?),
                remove_liquidity_policy: Some(policy_record_from_meteora_upgrade_step(
                    state,
                    shard,
                    meteora::MeteoraPolicyKind::RemoveLiquidity,
                )?),
                claim_fee_policy: Some(policy_record_from_meteora_upgrade_step(
                    state,
                    shard,
                    meteora::MeteoraPolicyKind::ClaimFees,
                )?),
            })
        })
        .collect::<Result<Vec<_>>>()?;
    {
        let record = state.meteora.as_mut().context("Meteora state is missing")?;
        record.policy_generation = meteora::METEORA_EXPANDED_POLICY_GENERATION;
        record.source_slot = expanded.source_slot;
        record.position_lower_bin_id = expanded.position_lower_bin_id;
        record.position_upper_bin_id = expanded.position_upper_bin_id;
        record.position_width = expanded.position_width;
        record.bin_arrays = expanded
            .bin_arrays
            .iter()
            .map(ToString::to_string)
            .collect();
        record.additional_policy_shards = additional_policy_shards;
    }
    state::save(path, state)?;
    meteora::validate_record(
        state.meteora.as_ref().context("Meteora state is missing")?,
        &expanded,
    )?;
    verify_all_meteora_policy_accounts(
        rpc,
        &expanded,
        settings,
        deployment.pubkey(),
        delegated.pubkey(),
    )?;
    println!(
        "meteora_policy_upgrade=PASS generation={} policies_added=6 total_meteora_policies=9 bin_arrays={}",
        meteora::METEORA_EXPANDED_POLICY_GENERATION,
        expanded.bin_arrays.len()
    );
    Ok(())
}

fn acquire_meteora_loyal_dust(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    const STEP: &str = "meteora-acquire-loyal-dust";
    let (_, _, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    ensure_meteora_record(path, state, &plan)?;
    let before = meteora_acquisition_observations(rpc, deployment.pubkey())?;
    ensure_meteora_live_step(path, state, STEP, before)?;

    if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, STEP)? {
        let after = verify_meteora_acquisition(
            rpc,
            deployment.pubkey(),
            &meteora_live_step(state, STEP)?.before,
        )?;
        return finalize_meteora_live_step(rpc, path, state, STEP, signature, after);
    }
    if meteora_live_step(state, STEP)?.status == PolicyStatus::Finalized {
        verify_meteora_acquisition(
            rpc,
            deployment.pubkey(),
            &meteora_live_step(state, STEP)?.before,
        )?;
        println!("{STEP}=PASS already_finalized=true");
        return Ok(());
    }

    let swap = meteora::build_direct_loyal_acquisition_swap(rpc, deployment.pubkey())?;
    if swap.amount_in_usdc_raw != meteora::LOYAL_ACQUIRE_USDC_RAW {
        bail!("direct Meteora dust acquisition changed the fixed input amount");
    }
    if observed(
        &meteora_live_step(state, STEP)?.before,
        "deployment_usdc_raw",
    )? < swap.amount_in_usdc_raw
    {
        bail!("deployment wallet has insufficient USDC for LOYAL dust acquisition");
    }
    {
        let step = meteora_live_step_mut(state, STEP)?;
        step.before.insert(
            "expected_usdc_input_raw".to_owned(),
            swap.amount_in_usdc_raw,
        );
        step.before.insert(
            "quoted_loyal_output_raw".to_owned(),
            swap.quoted_loyal_out_raw,
        );
        step.before.insert(
            "minimum_loyal_output_raw".to_owned(),
            swap.minimum_loyal_out_raw,
        );
        step.before
            .insert("quoted_fee_raw".to_owned(), swap.quoted_fee_raw);
        step.before.insert(
            "quoted_protocol_fee_raw".to_owned(),
            swap.quoted_protocol_fee_raw,
        );
    }
    state::save(path, state)?;
    let compute = ComputeBudgetInstruction::set_compute_unit_limit(1_400_000);
    let create_loyal_ata = meteora::create_deployment_loyal_ata_instruction(deployment.pubkey());
    let (transaction, blockhash, last_valid_block_height) = build_signed_transaction(
        rpc,
        &[compute, create_loyal_ata, swap.instruction.clone()],
        deployment,
    )?;
    let packet_size = bincode::serialized_size(&transaction)?;
    if packet_size > SOLANA_PACKET_DATA_SIZE {
        bail!("direct Meteora dust transaction exceeds Solana's packet limit");
    }
    let units = simulate_signed_transaction(rpc, &transaction, STEP)?;
    println!(
        "{STEP}_simulation=PASS packet_bytes={packet_size} units_consumed={units} route=direct-meteora-dlmm input_usdc_raw={} quoted_loyal_raw={} minimum_loyal_raw={} quoted_fee_raw={}",
        swap.amount_in_usdc_raw,
        swap.quoted_loyal_out_raw,
        swap.minimum_loyal_out_raw,
        swap.quoted_fee_raw,
    );
    let sent = send_meteora_live_transaction(
        rpc,
        path,
        state,
        STEP,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let before = meteora_live_step(state, STEP)?.before.clone();
    let after = verify_meteora_acquisition(rpc, deployment.pubkey(), &before)?;
    finalize_meteora_live_step(rpc, path, state, STEP, sent, after)
}

fn meteora_acquisition_observations(
    rpc: &RpcClient,
    deployment: Pubkey,
) -> Result<BTreeMap<String, u64>> {
    let deployment_usdc = derive_associated_token_address(deployment, USDC_MINT);
    let deployment_loyal = derive_associated_token_address(
        deployment,
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    );
    let mut observations = BTreeMap::new();
    observations.insert(
        "deployment_usdc_raw".to_owned(),
        token_account_amount(rpc, deployment_usdc, deployment, USDC_MINT)?.unwrap_or(0),
    );
    let loyal = token_account_amount(
        rpc,
        deployment_loyal,
        deployment,
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    )?;
    observations.insert(
        "deployment_loyal_exists".to_owned(),
        u64::from(loyal.is_some()),
    );
    observations.insert("deployment_loyal_raw".to_owned(), loyal.unwrap_or(0));
    Ok(observations)
}

fn verify_meteora_acquisition(
    rpc: &RpcClient,
    deployment: Pubkey,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = meteora_acquisition_observations(rpc, deployment)?;
    let input = before
        .get("expected_usdc_input_raw")
        .copied()
        .context("Meteora acquisition evidence is missing expected input")?;
    let minimum = before
        .get("minimum_loyal_output_raw")
        .copied()
        .context("Meteora acquisition evidence is missing minimum output")?;
    let before_usdc = before
        .get("deployment_usdc_raw")
        .copied()
        .context("Meteora acquisition evidence is missing before USDC")?;
    let after_usdc = after
        .get("deployment_usdc_raw")
        .copied()
        .context("Meteora acquisition evidence is missing after USDC")?;
    let before_loyal = before
        .get("deployment_loyal_raw")
        .copied()
        .context("Meteora acquisition evidence is missing before LOYAL")?;
    let after_loyal = after
        .get("deployment_loyal_raw")
        .copied()
        .context("Meteora acquisition evidence is missing after LOYAL")?;
    if before_usdc.checked_sub(input) != Some(after_usdc)
        || after_loyal.saturating_sub(before_loyal) < minimum
        || after.get("deployment_loyal_exists") != Some(&1)
    {
        bail!("direct Meteora LOYAL dust acquisition does not match exact RPC balance deltas");
    }
    Ok(after)
}

fn ensure_meteora_record(
    path: &std::path::PathBuf,
    state: &mut VaultState,
    plan: &meteora::MeteoraPlan,
) -> Result<()> {
    if state.meteora.is_none() {
        state.meteora = Some(meteora::record_from_plan(plan));
        state::save(path, state)?;
    }
    Ok(())
}

fn ensure_meteora_live_step(
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    before: BTreeMap<String, u64>,
) -> Result<()> {
    let steps = &mut state
        .meteora
        .as_mut()
        .context("Meteora state record is missing")?
        .live_steps;
    if steps.iter().all(|step| step.name != name) {
        steps.push(LiveStepRecord {
            name: name.to_owned(),
            status: PolicyStatus::Planned,
            pending_signature: None,
            last_valid_block_height: None,
            finalized_signature: None,
            finalized_slot: None,
            before,
            after: BTreeMap::new(),
        });
        state::save(path, state)?;
    }
    Ok(())
}

fn meteora_live_step<'a>(state: &'a VaultState, name: &str) -> Result<&'a LiveStepRecord> {
    state
        .meteora
        .as_ref()
        .and_then(|record| record.live_steps.iter().find(|step| step.name == name))
        .with_context(|| format!("Meteora live step {name} is missing"))
}

fn meteora_live_step_mut<'a>(
    state: &'a mut VaultState,
    name: &str,
) -> Result<&'a mut LiveStepRecord> {
    state
        .meteora
        .as_mut()
        .and_then(|record| record.live_steps.iter_mut().find(|step| step.name == name))
        .with_context(|| format!("Meteora live step {name} is missing"))
}

fn recover_finalized_meteora_live_step(
    rpc: &RpcClient,
    state: &VaultState,
    name: &str,
) -> Result<Option<Signature>> {
    let step = meteora_live_step(state, name)?;
    if step.status == PolicyStatus::Finalized {
        return Ok(None);
    }
    let Some(signature) = step.pending_signature.as_deref() else {
        return Ok(None);
    };
    let signature = Signature::from_str(signature)?;
    let status = rpc
        .get_signature_statuses(&[signature])?
        .value
        .into_iter()
        .next()
        .flatten();
    if let Some(status) = status {
        if let Some(error) = status.err {
            bail!("recorded {name} transaction failed on chain: {error:?}");
        }
        if status.satisfies_commitment(CommitmentConfig::finalized()) {
            return Ok(Some(signature));
        }
        bail!("recorded {name} transaction is not finalized yet");
    }
    let last_valid = step
        .last_valid_block_height
        .context("pending Meteora step is missing last valid block height")?;
    if rpc.get_block_height()? <= last_valid {
        bail!("recorded {name} signature is still live but not visible; retry later");
    }
    if let Ok(transaction) = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    ) {
        if transaction
            .transaction
            .meta
            .as_ref()
            .and_then(|meta| meta.err.as_ref())
            .is_some()
        {
            bail!("recorded {name} transaction finalized with an error");
        }
        return Ok(Some(signature));
    }
    Ok(None)
}

fn finalize_meteora_live_step(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    signature: Signature,
    after: BTreeMap<String, u64>,
) -> Result<()> {
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let step = meteora_live_step_mut(state, name)?;
    step.status = PolicyStatus::Finalized;
    step.finalized_signature = Some(signature.to_string());
    step.finalized_slot = Some(transaction.slot);
    step.after = after;
    state::save(path, state)?;
    println!(
        "{name}=PASS signature={signature} slot={}",
        transaction.slot
    );
    Ok(())
}

fn setup_meteora_accounts(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    const STEP: &str = "meteora-account-setup";
    let (settings, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    ensure_meteora_record(path, state, &plan)?;
    require_finalized_meteora_live_step(state, "meteora-acquire-loyal-dust")?;
    let before = meteora_setup_observations(rpc, deployment.pubkey(), vault, &plan)?;
    ensure_meteora_live_step(path, state, STEP, before)?;
    let before = meteora_live_step(state, STEP)?.before.clone();

    if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, STEP)? {
        let after = verify_meteora_setup(rpc, deployment.pubkey(), vault, &plan, &before)?;
        return finalize_meteora_live_step(rpc, path, state, STEP, signature, after);
    }
    if meteora_live_step(state, STEP)?.status == PolicyStatus::Finalized {
        verify_meteora_setup(rpc, deployment.pubkey(), vault, &plan, &before)?;
        println!("{STEP}=PASS already_finalized=true");
        return Ok(());
    }
    if before.get("position_exists") != Some(&0) || before.get("vault_loyal_exists") != Some(&0) {
        bail!("Meteora setup accounts existed before finalized setup evidence");
    }
    if before.get("deployment_loyal_raw").copied().unwrap_or(0) < METEORA_TEST_LOYAL_RAW {
        bail!("deployment wallet has insufficient acquired LOYAL dust for Meteora setup");
    }

    let mut transaction_accounts = Vec::new();
    let compiled = meteora::setup_inner_instructions(vault, &plan)?
        .into_iter()
        .map(|instruction| compile_squads_inner_instruction(&mut transaction_accounts, instruction))
        .collect();
    let setup = execute_sync_transaction_instruction(
        settings,
        deployment.pubkey(),
        VAULT_INDEX,
        compiled,
        transaction_accounts,
    );
    let fund_vault =
        system_instruction::transfer(&deployment.pubkey(), &vault, METEORA_SETUP_VAULT_LAMPORTS);
    let deployment_loyal = derive_associated_token_address(
        deployment.pubkey(),
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    );
    let fund_loyal = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &deployment_loyal,
        &loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        &plan.vault_loyal,
        &deployment.pubkey(),
        &[],
        METEORA_TEST_LOYAL_RAW,
        6,
    )?;
    let compute = ComputeBudgetInstruction::set_compute_unit_limit(500_000);
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &[compute, fund_vault, setup, fund_loyal], deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, STEP)?;
    println!("{STEP}_simulation=PASS units_consumed={units}");
    println!(
        "setup_exception_path=settings signer={}",
        deployment.pubkey()
    );
    println!("inner_rent_payer={vault}");
    let signature = send_meteora_live_transaction(
        rpc,
        path,
        state,
        STEP,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_meteora_setup(rpc, deployment.pubkey(), vault, &plan, &before)?;
    finalize_meteora_live_step(rpc, path, state, STEP, signature, after)
}

const METEORA_POSITION_EXPAND_TARGETS: [i32; 2] = [-77, 0];
const METEORA_POSITION_EXPAND_VAULT_CUSHION_LAMPORTS: u64 = 10_000_000;

fn meteora_position_expand_step_name(target_upper_bin_id: i32) -> String {
    let target = if target_upper_bin_id < 0 {
        format!("neg{}", target_upper_bin_id.unsigned_abs())
    } else {
        target_upper_bin_id.to_string()
    };
    format!("meteora-position-expand-upper-to-bin-{target}")
}

fn next_meteora_position_expand_target(current_upper_bin_id: i32) -> Option<i32> {
    METEORA_POSITION_EXPAND_TARGETS
        .into_iter()
        .find(|target| *target > current_upper_bin_id)
}

fn simulate_meteora_position_expand(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (settings, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    let Some(target_upper_bin_id) = next_meteora_position_expand_target(plan.position_upper_bin_id)
    else {
        println!(
            "module=meteora-position-expand-simulation verdict=PASS already_expanded=true transaction_sent=false"
        );
        return Ok(());
    };
    let step_name = meteora_position_expand_step_name(target_upper_bin_id);
    let before = meteora_position_expand_observations(rpc, deployment.pubkey(), vault, &plan)?;
    let (transaction, _, _, vault_funding) = build_meteora_position_expand_transaction(
        rpc,
        settings,
        vault,
        &plan,
        deployment,
        &before,
        target_upper_bin_id,
    )?;
    let packet_size = bincode::serialized_size(&transaction)?;
    if packet_size > SOLANA_PACKET_DATA_SIZE {
        bail!("Meteora position expansion exceeds Solana's packet limit");
    }
    let units = simulate_signed_transaction(rpc, &transaction, &step_name)?;
    println!("module=meteora-position-expand-simulation verdict=PASS");
    println!("position={}", plan.position);
    println!(
        "bounds_before={}..={} bounds_after={}..={}",
        plan.position_lower_bin_id,
        plan.position_upper_bin_id,
        meteora::POSITION_LOWER_BIN_ID,
        target_upper_bin_id
    );
    println!(
        "remaining_final_upper_bin={}",
        meteora::POSITION_TARGET_UPPER_BIN_ID
    );
    println!("vault_funding_lamports={vault_funding}");
    println!("packet_bytes={packet_size}");
    println!("units_consumed={units}");
    println!("transaction_sent=false");
    Ok(())
}

fn expand_meteora_position(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    for target_upper_bin_id in METEORA_POSITION_EXPAND_TARGETS {
        let (settings, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
        let step_name = meteora_position_expand_step_name(target_upper_bin_id);
        if plan.position_upper_bin_id > target_upper_bin_id {
            if meteora_live_step(state, &step_name)?.status != PolicyStatus::Finalized {
                bail!("Meteora position passed resize checkpoint {target_upper_bin_id} without finalized evidence");
            }
            continue;
        }
        if plan.position_upper_bin_id == target_upper_bin_id
            && meteora_live_step(state, &step_name)?.status == PolicyStatus::Finalized
        {
            continue;
        }
        expand_meteora_position_step(
            rpc,
            path,
            state,
            deployment,
            settings,
            vault,
            &plan,
            target_upper_bin_id,
            &step_name,
        )?;
    }
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    let position = meteora::load_position_snapshot(rpc, plan.position, vault)?
        .context("Meteora position is absent after expansion")?;
    if position.upper_bin_id != meteora::POSITION_TARGET_UPPER_BIN_ID {
        bail!("Meteora position did not reach the approved upper target");
    }
    println!(
        "module=meteora-position-expand verdict=PASS position={} bounds={}..={} width={} rent_lamports={}",
        plan.position,
        position.lower_bin_id,
        position.upper_bin_id,
        position.upper_bin_id - position.lower_bin_id + 1,
        position.lamports
    );
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn expand_meteora_position_step(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    settings: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    target_upper_bin_id: i32,
    step_name: &str,
) -> Result<()> {
    ensure_meteora_record(path, state, plan)?;
    let before = meteora_position_expand_observations(rpc, deployment.pubkey(), vault, plan)?;
    ensure_meteora_live_step(path, state, step_name, before)?;
    let before = meteora_live_step(state, step_name)?.before.clone();
    if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, step_name)? {
        let after = verify_meteora_position_expand(
            rpc,
            deployment.pubkey(),
            vault,
            plan,
            &before,
            target_upper_bin_id,
        )?;
        update_meteora_position_record(state, &after, target_upper_bin_id)?;
        return finalize_meteora_live_step(rpc, path, state, step_name, signature, after);
    }
    if meteora_live_step(state, step_name)?.status == PolicyStatus::Finalized {
        verify_meteora_position_expand(
            rpc,
            deployment.pubkey(),
            vault,
            plan,
            &before,
            target_upper_bin_id,
        )?;
        println!("{step_name}=PASS already_finalized=true");
        return Ok(());
    }
    if plan.position_upper_bin_id == target_upper_bin_id {
        bail!(
            "Meteora position reached checkpoint {target_upper_bin_id} without finalized evidence"
        );
    }
    let (transaction, blockhash, last_valid_block_height, vault_funding) =
        build_meteora_position_expand_transaction(
            rpc,
            settings,
            vault,
            plan,
            deployment,
            &before,
            target_upper_bin_id,
        )?;
    let packet_size = bincode::serialized_size(&transaction)?;
    if packet_size > SOLANA_PACKET_DATA_SIZE {
        bail!("Meteora position expansion exceeds Solana's packet limit");
    }
    let units = simulate_signed_transaction(rpc, &transaction, step_name)?;
    println!(
        "{step_name}_simulation=PASS packet_bytes={packet_size} units_consumed={units} vault_funding_lamports={vault_funding}"
    );
    println!(
        "setup_exception_path=settings signer={}",
        deployment.pubkey()
    );
    println!("inner_rent_payer={vault}");
    let signature = send_meteora_live_transaction(
        rpc,
        path,
        state,
        step_name,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_meteora_position_expand(
        rpc,
        deployment.pubkey(),
        vault,
        plan,
        &before,
        target_upper_bin_id,
    )?;
    update_meteora_position_record(state, &after, target_upper_bin_id)?;
    finalize_meteora_live_step(rpc, path, state, step_name, signature, after)
}

fn build_meteora_position_expand_transaction(
    rpc: &RpcClient,
    settings: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    deployment: &solana_sdk::signature::Keypair,
    before: &BTreeMap<String, u64>,
    target_upper_bin_id: i32,
) -> Result<(Transaction, solana_sdk::hash::Hash, u64, u64)> {
    if observed(before, "position_nonzero_liquidity_bins")? != 0
        || observed(before, "position_pending_fee_loyal_raw")? != 0
        || observed(before, "position_pending_fee_usdc_raw")? != 0
    {
        bail!("Meteora position must be empty with no pending fees before expansion");
    }
    let inner_instruction =
        meteora::expand_position_upper_instruction(vault, plan, target_upper_bin_id)?;
    let mut transaction_accounts = Vec::new();
    let compiled = vec![compile_squads_inner_instruction(
        &mut transaction_accounts,
        inner_instruction,
    )];
    let resize = execute_sync_transaction_instruction(
        settings,
        deployment.pubkey(),
        VAULT_INDEX,
        compiled,
        transaction_accounts,
    );

    let target_width = target_upper_bin_id
        .checked_sub(meteora::POSITION_LOWER_BIN_ID)
        .and_then(|delta| delta.checked_add(1))
        .context("Meteora resize target width overflow")?;
    let target_data_len = meteora::position_data_len_for_width(target_width)?;
    let target_rent = rpc
        .get_minimum_balance_for_rent_exemption(target_data_len)
        .context("quote target Meteora position rent")?;
    let position_top_up = target_rent
        .checked_sub(observed(before, "position_lamports")?)
        .context("Meteora position already exceeds target rent quote")?;
    let required_vault_balance = position_top_up
        .checked_add(METEORA_POSITION_EXPAND_VAULT_CUSHION_LAMPORTS)
        .context("Meteora resize vault balance overflow")?;
    let vault_funding = required_vault_balance.saturating_sub(observed(before, "vault_lamports")?);
    if observed(before, "deployment_lamports")?
        < vault_funding
            .checked_add(1_000_000)
            .context("Meteora deployment funding threshold overflow")?
    {
        bail!("deployment signer has insufficient SOL for the approved position expansion");
    }

    let mut outer_instructions = vec![ComputeBudgetInstruction::set_compute_unit_limit(1_400_000)];
    if vault_funding > 0 {
        outer_instructions.push(system_instruction::transfer(
            &deployment.pubkey(),
            &vault,
            vault_funding,
        ));
    }
    outer_instructions.push(resize);
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &outer_instructions, deployment)?;
    Ok((
        transaction,
        blockhash,
        last_valid_block_height,
        vault_funding,
    ))
}

fn meteora_position_expand_observations(
    rpc: &RpcClient,
    deployment: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
) -> Result<BTreeMap<String, u64>> {
    let position = meteora::load_position_snapshot(rpc, plan.position, vault)?
        .context("Meteora position is absent before expansion")?;
    let mut observations = BTreeMap::new();
    observations.insert(
        "deployment_lamports".to_owned(),
        rpc.get_balance(&deployment)?,
    );
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    observations.insert("position_lamports".to_owned(), position.lamports);
    observations.insert("position_data_len".to_owned(), position.data_len as u64);
    observations.insert(
        "position_width".to_owned(),
        u64::try_from(position.upper_bin_id - position.lower_bin_id + 1)
            .context("convert Meteora position width")?,
    );
    observations.insert(
        "position_nonzero_liquidity_bins".to_owned(),
        position.nonzero_liquidity_bins,
    );
    observations.insert(
        "position_pending_fee_loyal_raw".to_owned(),
        position.pending_fee_x,
    );
    observations.insert(
        "position_pending_fee_usdc_raw".to_owned(),
        position.pending_fee_y,
    );
    Ok(observations)
}

fn verify_meteora_position_expand(
    rpc: &RpcClient,
    deployment: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    before: &BTreeMap<String, u64>,
    target_upper_bin_id: i32,
) -> Result<BTreeMap<String, u64>> {
    let after = meteora_position_expand_observations(rpc, deployment, vault, plan)?;
    let position = meteora::load_position_snapshot(rpc, plan.position, vault)?
        .context("Meteora position disappeared after expansion")?;
    let target_width = target_upper_bin_id
        .checked_sub(meteora::POSITION_LOWER_BIN_ID)
        .and_then(|delta| delta.checked_add(1))
        .context("Meteora verified target width overflow")?;
    let expected_data_len = meteora::position_data_len_for_width(target_width)?;
    let expected_rent = rpc.get_minimum_balance_for_rent_exemption(expected_data_len)?;
    if position.lower_bin_id != meteora::POSITION_LOWER_BIN_ID
        || position.upper_bin_id != target_upper_bin_id
        || position.data_len != expected_data_len
        || position.lamports != expected_rent
        || position.nonzero_liquidity_bins != 0
        || position.pending_fee_x != 0
        || position.pending_fee_y != 0
    {
        bail!("expanded Meteora position does not match the approved empty target envelope");
    }
    let expected_position_delta = expected_rent
        .checked_sub(observed(before, "position_lamports")?)
        .context("Meteora position rent delta underflow")?;
    if observed(&after, "position_lamports")?.checked_sub(observed(before, "position_lamports")?)
        != Some(expected_position_delta)
    {
        bail!("Meteora position rent delta does not match the live mainnet quote");
    }
    Ok(after)
}

fn update_meteora_position_record(
    state: &mut VaultState,
    after: &BTreeMap<String, u64>,
    target_upper_bin_id: i32,
) -> Result<()> {
    let record = state
        .meteora
        .as_mut()
        .context("Meteora state record is missing")?;
    record.position_lower_bin_id = meteora::POSITION_LOWER_BIN_ID;
    record.position_upper_bin_id = target_upper_bin_id;
    record.position_width = i32::try_from(observed(after, "position_width")?)
        .context("convert finalized Meteora position width")?;
    Ok(())
}

fn meteora_setup_observations(
    rpc: &RpcClient,
    deployment: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
) -> Result<BTreeMap<String, u64>> {
    let deployment_loyal = derive_associated_token_address(
        deployment,
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    );
    let mut observations = BTreeMap::new();
    observations.insert(
        "deployment_lamports".to_owned(),
        rpc.get_balance(&deployment)?,
    );
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    observations.insert(
        "deployment_loyal_raw".to_owned(),
        token_account_amount(
            rpc,
            deployment_loyal,
            deployment,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .unwrap_or(0),
    );
    let vault_loyal = token_account_amount(
        rpc,
        plan.vault_loyal,
        vault,
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    )?;
    observations.insert(
        "vault_loyal_exists".to_owned(),
        u64::from(vault_loyal.is_some()),
    );
    observations.insert("vault_loyal_raw".to_owned(), vault_loyal.unwrap_or(0));
    observations.insert(
        "vault_loyal_account_lamports".to_owned(),
        optional_account_lamports(rpc, plan.vault_loyal)?,
    );
    observations.insert(
        "vault_usdc_raw".to_owned(),
        token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?
            .context("vault USDC account is absent before Meteora setup")?,
    );
    let position = meteora::load_position_snapshot(rpc, plan.position, vault)?;
    observations.insert("position_exists".to_owned(), u64::from(position.is_some()));
    observations.insert(
        "position_lamports".to_owned(),
        position.map(|snapshot| snapshot.lamports).unwrap_or(0),
    );
    observations.insert(
        "position_data_len".to_owned(),
        position
            .map(|snapshot| snapshot.data_len as u64)
            .unwrap_or(0),
    );
    observations.insert(
        "position_nonzero_liquidity_bins".to_owned(),
        position
            .map(|snapshot| snapshot.nonzero_liquidity_bins)
            .unwrap_or(0),
    );
    observations.insert(
        "position_pending_fee_loyal_raw".to_owned(),
        position.map(|snapshot| snapshot.pending_fee_x).unwrap_or(0),
    );
    observations.insert(
        "position_pending_fee_usdc_raw".to_owned(),
        position.map(|snapshot| snapshot.pending_fee_y).unwrap_or(0),
    );
    for (index, bin_array) in plan.bin_arrays.iter().enumerate() {
        observations.insert(
            format!("bin_array_{index}_exists"),
            u64::from(account_exists_with_owner(
                rpc,
                *bin_array,
                loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID,
            )?),
        );
    }
    Ok(observations)
}

fn verify_meteora_setup(
    rpc: &RpcClient,
    deployment: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = meteora_setup_observations(rpc, deployment, vault, plan)?;
    for index in 0..plan.bin_arrays.len() {
        if after.get(&format!("bin_array_{index}_exists")) != Some(&1) {
            bail!("required Meteora BinArray {index} did not persist through setup");
        }
    }
    let before_deployment_loyal = before
        .get("deployment_loyal_raw")
        .copied()
        .context("missing before deployment LOYAL")?;
    let after_deployment_loyal = after
        .get("deployment_loyal_raw")
        .copied()
        .context("missing after deployment LOYAL")?;
    let before_vault_lamports = before
        .get("vault_lamports")
        .copied()
        .context("missing before vault lamports")?;
    let after_vault_lamports = after
        .get("vault_lamports")
        .copied()
        .context("missing after vault lamports")?;
    let position_lamports = after
        .get("position_lamports")
        .copied()
        .context("missing position rent")?;
    let loyal_ata_lamports = after
        .get("vault_loyal_account_lamports")
        .copied()
        .context("missing vault LOYAL ATA rent")?;
    if before_deployment_loyal.checked_sub(METEORA_TEST_LOYAL_RAW) != Some(after_deployment_loyal)
        || after.get("vault_loyal_exists") != Some(&1)
        || after.get("vault_loyal_raw") != Some(&METEORA_TEST_LOYAL_RAW)
        || before.get("vault_usdc_raw") != after.get("vault_usdc_raw")
        || after.get("position_exists") != Some(&1)
        || after.get("position_nonzero_liquidity_bins") != Some(&0)
        || after.get("position_pending_fee_loyal_raw") != Some(&0)
        || after.get("position_pending_fee_usdc_raw") != Some(&0)
        || before_vault_lamports
            .checked_add(METEORA_SETUP_VAULT_LAMPORTS)
            .and_then(|value| value.checked_sub(position_lamports))
            .and_then(|value| value.checked_sub(loyal_ata_lamports))
            != Some(after_vault_lamports)
    {
        bail!("Meteora setup does not match the exact vault-paid rent and token manifest");
    }
    Ok(after)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum MeteoraExecutionKind {
    AddA,
    RemoveA,
    AddB,
    RemoveB,
}

impl MeteoraExecutionKind {
    fn step_name(self) -> &'static str {
        match self {
            Self::AddA => "meteora-add-a",
            Self::RemoveA => "meteora-remove-a",
            Self::AddB => "meteora-add-b",
            Self::RemoveB => "meteora-remove-b",
        }
    }

    fn prerequisite(self) -> &'static str {
        match self {
            Self::AddA => "meteora-account-setup",
            Self::RemoveA => Self::AddA.step_name(),
            Self::AddB => Self::RemoveA.step_name(),
            Self::RemoveB => "meteora-generate-fees",
        }
    }

    fn policy_kind(self) -> meteora::MeteoraPolicyKind {
        match self {
            Self::AddA | Self::AddB => meteora::MeteoraPolicyKind::AddLiquidity,
            Self::RemoveA | Self::RemoveB => meteora::MeteoraPolicyKind::RemoveLiquidity,
        }
    }

    fn is_add(self) -> bool {
        matches!(self, Self::AddA | Self::AddB)
    }

    fn recorded_range(self, state: &VaultState) -> Result<meteora::BinRange> {
        let record = state
            .meteora
            .as_ref()
            .context("Meteora state record is missing")?;
        let (min, max) = match self {
            Self::AddA | Self::RemoveA => (
                record.strategy_range_a_min_bin_id,
                record.strategy_range_a_max_bin_id,
            ),
            Self::AddB | Self::RemoveB => (
                record.strategy_range_b_min_bin_id,
                record.strategy_range_b_max_bin_id,
            ),
        };
        Ok(meteora::BinRange { min, max })
    }
}

fn simulate_meteora_execution(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: MeteoraExecutionKind,
) -> Result<()> {
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, kind.prerequisite())?;
    let before = meteora_liquidity_observations(rpc, vault, &plan)?;
    validate_meteora_liquidity_before(kind, &before)?;
    let range = kind.recorded_range(state)?;
    let (execute, policy_address) = build_meteora_liquidity_policy_execution(
        state,
        delegated.pubkey(),
        vault,
        &plan,
        kind,
        plan.active_bin_id,
        range,
    )?;
    let compute = ComputeBudgetInstruction::set_compute_unit_limit(600_000);
    let (transaction, _, _) = build_signed_transaction(rpc, &[compute, execute], delegated)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.step_name())?;
    println!("module={} simulation verdict=PASS", kind.step_name());
    println!("policy_execution_path={policy_address}");
    println!("policy_signer={}", delegated.pubkey());
    println!("strategy_range={}..={}", range.min, range.max);
    println!("units_consumed={units}");
    println!("transaction_sent=false");
    Ok(())
}

fn simulate_meteora_adversarial_matrix(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, "meteora-claim-fees")?;
    let before = meteora_liquidity_observations(rpc, vault, &plan)?;
    if observed(&before, "position_nonzero_liquidity_bins")? != 0
        || observed(&before, "position_pending_fee_loyal_raw")? != 0
        || observed(&before, "position_pending_fee_usdc_raw")? != 0
    {
        bail!("Meteora adversarial matrix requires the finalized empty position");
    }

    let canonical_inner = meteora::add_liquidity_instruction(
        vault,
        &plan,
        METEORA_LIQUIDITY_TEST_LOYAL_RAW,
        METEORA_LIQUIDITY_TEST_USDC_RAW,
        plan.active_bin_id,
        plan.range_b,
    )?;
    let canonical = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::AddB,
        canonical_inner.clone(),
    )?;
    simulate_meteora_matrix_case(rpc, delegated, "canonical-b", &[canonical], true, 1, &[])?;

    for (label, range) in [
        (
            "generation-2-shard-1",
            meteora::BinRange {
                min: -100,
                max: -90,
            },
        ),
        ("generation-2-shard-2", meteora::BinRange { min: 0, max: 0 }),
    ] {
        let (add, add_policy) = build_meteora_liquidity_policy_execution(
            state,
            delegated.pubkey(),
            vault,
            &plan,
            MeteoraExecutionKind::AddB,
            plan.active_bin_id,
            range,
        )?;
        let (remove, remove_policy) = build_meteora_liquidity_policy_execution(
            state,
            delegated.pubkey(),
            vault,
            &plan,
            MeteoraExecutionKind::RemoveB,
            plan.active_bin_id,
            range,
        )?;
        println!(
            "meteora_generation_2_case={label}-atomic-add-remove range={}..={} add_policy={add_policy} remove_policy={remove_policy}",
            range.min, range.max
        );
        simulate_meteora_matrix_case(
            rpc,
            delegated,
            &format!("{label}-atomic-add-remove"),
            &[add, remove],
            true,
            2,
            &[],
        )?;

        let (claim, claim_policy) =
            build_meteora_claim_policy_execution(state, delegated.pubkey(), vault, &plan, range)?;
        println!(
            "meteora_generation_2_case={label}-claim range={}..={} policy={claim_policy}",
            range.min, range.max
        );
        simulate_meteora_matrix_case(
            rpc,
            delegated,
            &format!("{label}-claim"),
            &[claim],
            true,
            1,
            &[],
        )?;
    }

    let mut noncontinuous_inner = canonical_inner.clone();
    if noncontinuous_inner.accounts.len() != 16 || plan.bin_arrays.len() < 3 {
        bail!("Meteora add account graph changed from the reviewed layout");
    }
    noncontinuous_inner.accounts[14].pubkey = plan.bin_arrays[0];
    noncontinuous_inner.accounts[15].pubkey = plan.bin_arrays[2];
    let noncontinuous = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::AddB,
        noncontinuous_inner,
    )?;
    simulate_meteora_matrix_case(
        rpc,
        delegated,
        "noncontinuous-arrays-minus4-minus2",
        &[noncontinuous],
        false,
        1,
        &[
            "InvalidBinArray",
            "BinArraysMustBeContinuous",
            "AccountNotEnoughKeys",
            "3005",
            "0xbbd",
            "6027",
            "6028",
            "0x178b",
            "0x178c",
        ],
    )?;

    let mut duplicate_inner = canonical_inner.clone();
    duplicate_inner.accounts[14].pubkey = plan.bin_arrays[1];
    duplicate_inner.accounts[15].pubkey = plan.bin_arrays[1];
    let duplicate = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::AddB,
        duplicate_inner,
    )?;
    simulate_meteora_matrix_case(
        rpc,
        delegated,
        "duplicate-arrays-minus3-minus3",
        &[duplicate],
        false,
        1,
        &[
            "InvalidBinArray",
            "BinArraysMustBeContinuous",
            "AccountBorrowFailed",
            "already borrowed",
            "AccountNotEnoughKeys",
            "3005",
            "0xbbd",
            "6027",
            "6028",
            "0x178b",
            "0x178c",
        ],
    )?;

    let mut out_of_position_inner = canonical_inner.clone();
    overwrite_i32(&mut out_of_position_inner.data, 32, -238)?;
    overwrite_i32(&mut out_of_position_inner.data, 36, plan.range_b.max)?;
    let out_of_position = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::AddB,
        out_of_position_inner,
    )?;
    simulate_meteora_matrix_case(
        rpc,
        delegated,
        "out-of-position-range",
        &[out_of_position],
        false,
        1,
        &[
            "InvalidPosition",
            "BinIdOutOfBound",
            "InvalidBinId",
            "InvalidStrategyParameters",
            "6001",
            "6008",
            "6038",
            "6054",
            "0x1778",
            "0x1771",
            "0x1796",
            "0x17a6",
        ],
    )?;

    let mut inverted_inner = canonical_inner.clone();
    overwrite_i32(&mut inverted_inner.data, 32, plan.range_b.max)?;
    overwrite_i32(&mut inverted_inner.data, 36, plan.range_b.min)?;
    let inverted = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::AddB,
        inverted_inner,
    )?;
    simulate_meteora_matrix_case(
        rpc,
        delegated,
        "inverted-range",
        &[inverted],
        false,
        1,
        &[
            "InvalidStrategyParameters",
            "attempt to subtract with overflow",
            "weight_to_amounts.rs",
            "6054",
            "0x17a6",
        ],
    )?;

    let atomic_add = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::AddB,
        canonical_inner,
    )?;
    let mut excessive_bps_inner =
        meteora::remove_liquidity_instruction(vault, &plan, plan.range_b, 10_000)?;
    if excessive_bps_inner.data.len() != 22 {
        bail!("Meteora remove instruction changed from the reviewed wire layout");
    }
    excessive_bps_inner.data[16..18].copy_from_slice(&10_001_u16.to_le_bytes());
    let excessive_bps = wrap_meteora_inner_policy_execution(
        state,
        delegated.pubkey(),
        MeteoraExecutionKind::RemoveB,
        excessive_bps_inner,
    )?;
    simulate_meteora_matrix_case(
        rpc,
        delegated,
        "atomic-add-then-remove-10001-bps",
        &[atomic_add, excessive_bps],
        false,
        2,
        &["InvalidBps", "InvalidBasisPoint", "6017", "0x1781"],
    )?;

    let after = meteora_liquidity_observations(rpc, vault, &plan)?;
    if before != after {
        bail!("signed-unsent Meteora simulations unexpectedly changed live state");
    }
    println!("module=meteora-adversarial-simulations verdict=PASS transaction_sent=false");
    Ok(())
}

fn wrap_meteora_inner_policy_execution(
    state: &VaultState,
    delegated: Pubkey,
    kind: MeteoraExecutionKind,
    inner: Instruction,
) -> Result<Instruction> {
    let record = meteora::policy_record(
        state
            .meteora
            .as_ref()
            .context("Meteora state record is missing")?,
        kind.policy_kind(),
    )
    .context("Meteora execution policy record is missing")?;
    if record.status != PolicyStatus::Finalized {
        bail!("Meteora execution policy is not finalized");
    }
    let policy = Pubkey::from_str(&record.policy)?;
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner);
    Ok(execute_program_interaction_policy_instruction(
        policy,
        delegated,
        VAULT_INDEX,
        vec![compiled],
        vec![0],
        transaction_accounts,
    ))
}

fn simulate_meteora_matrix_case(
    rpc: &RpcClient,
    delegated: &solana_sdk::signature::Keypair,
    label: &str,
    executions: &[Instruction],
    expect_success: bool,
    minimum_dlmm_invocations: usize,
    expected_error_markers: &[&str],
) -> Result<()> {
    let compute_limit = if executions.len() > 1 {
        1_400_000
    } else {
        600_000
    };
    let compute = ComputeBudgetInstruction::set_compute_unit_limit(compute_limit);
    let mut instructions = Vec::with_capacity(executions.len() + 1);
    instructions.push(compute);
    instructions.extend_from_slice(executions);
    let (transaction, _, _) = build_signed_transaction(rpc, &instructions, delegated)?;
    let simulation = rpc.simulate_transaction_with_config(
        &transaction,
        RpcSimulateTransactionConfig {
            sig_verify: true,
            replace_recent_blockhash: false,
            commitment: Some(CommitmentConfig::finalized()),
            ..RpcSimulateTransactionConfig::default()
        },
    )?;
    let packet_bytes = bincode::serialized_size(&transaction)?;
    if packet_bytes > SOLANA_PACKET_DATA_SIZE {
        bail!("{label} is {packet_bytes} bytes and cannot prove the required atomic mainnet path");
    }
    let logs = simulation.value.logs.unwrap_or_default();
    let squads_invoke_indices = logs
        .iter()
        .enumerate()
        .filter_map(|(index, line)| {
            line.contains(&format!(
                "Program {} invoke [1]",
                loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID
            ))
            .then_some(index)
        })
        .collect::<Vec<_>>();
    let dlmm_invoke_indices = logs
        .iter()
        .enumerate()
        .filter_map(|(index, line)| {
            line.contains(&format!(
                "Program {} invoke [2]",
                loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID
            ))
            .then_some(index)
        })
        .collect::<Vec<_>>();
    if squads_invoke_indices.is_empty() || dlmm_invoke_indices.len() < minimum_dlmm_invocations {
        bail!("{label} did not prove Squads accepted the payload and invoked DLMM");
    }
    for (ordinal, dlmm_index) in dlmm_invoke_indices
        .iter()
        .take(minimum_dlmm_invocations)
        .enumerate()
    {
        let prior_dlmm = ordinal
            .checked_sub(1)
            .map(|prior| dlmm_invoke_indices[prior])
            .unwrap_or(0);
        if !squads_invoke_indices
            .iter()
            .any(|squads_index| *squads_index >= prior_dlmm && *squads_index < *dlmm_index)
        {
            bail!(
                "{label} log order does not show Squads invoking DLMM execution {ordinal}; logs={}",
                logs.join(" || ")
            );
        }
    }
    if expect_success {
        if let Some(error) = simulation.value.err {
            bail!("{label} unexpectedly failed after reaching DLMM: {error:?}");
        }
        let dlmm_success = logs.iter().enumerate().any(|(index, line)| {
            index > dlmm_invoke_indices[0]
                && line.contains(&format!(
                    "Program {} success",
                    loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID
                ))
        });
        if !dlmm_success {
            bail!("{label} did not show a successful DLMM return after invocation");
        }
        println!(
            "meteora_adversarial_case={label} expected=PASS observed=PASS dlmm_invocations={dlmm_invocations} packet_bytes={}",
            packet_bytes,
            dlmm_invocations = dlmm_invoke_indices.len(),
        );
    } else {
        let error = simulation
            .value
            .err
            .with_context(|| format!("{label} unexpectedly passed"))?;
        let relevant_invoke = dlmm_invoke_indices[minimum_dlmm_invocations - 1];
        let marker_index = logs.iter().enumerate().find_map(|(index, line)| {
            (index > relevant_invoke
                && expected_error_markers
                    .iter()
                    .any(|marker| line.contains(marker)))
            .then_some(index)
        });
        let marker_index = marker_index.with_context(|| {
            format!(
                "{label} reached DLMM but did not emit one of the reviewed errors: {}; log_tail={}",
                expected_error_markers.join("|"),
                logs[relevant_invoke..].join(" || ")
            )
        })?;
        let dlmm_failed_index = logs.iter().enumerate().find_map(|(index, line)| {
            (index > marker_index
                && line.contains(&format!(
                    "Program {} failed",
                    loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID
                )))
            .then_some(index)
        });
        if dlmm_failed_index.is_none() {
            bail!("{label} failed without the expected DLMM runtime rejection");
        }
        if executions.len() > 1 {
            let first_success = logs.iter().enumerate().any(|(index, line)| {
                index > dlmm_invoke_indices[0]
                    && index < dlmm_invoke_indices[1]
                    && line.contains(&format!(
                        "Program {} success",
                        loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID
                    ))
            });
            if !first_success || marker_index <= dlmm_invoke_indices[1] {
                bail!(
                    "{label} did not prove atomic first-DLMM success followed by second-DLMM rejection"
                );
            }
        }
        println!(
            "meteora_adversarial_case={label} expected=DLMM_REJECT observed=DLMM_REJECT error={error:?} dlmm_invocations={dlmm_invocations} packet_bytes={}",
            packet_bytes,
            dlmm_invocations = dlmm_invoke_indices.len(),
        );
    }
    Ok(())
}

fn overwrite_i32(data: &mut [u8], offset: usize, value: i32) -> Result<()> {
    let target = data
        .get_mut(offset..offset + 4)
        .context("Meteora instruction is shorter than the reviewed range offset")?;
    target.copy_from_slice(&value.to_le_bytes());
    Ok(())
}

fn execute_meteora_liquidity_step(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: MeteoraExecutionKind,
) -> Result<()> {
    let step_name = kind.step_name();
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, kind.prerequisite())?;
    let mut before = meteora_liquidity_observations(rpc, vault, &plan)?;
    let intended_range = kind.recorded_range(state)?;
    before.insert(
        "range_min_i32_bits".to_owned(),
        u64::from(intended_range.min as u32),
    );
    before.insert(
        "range_max_i32_bits".to_owned(),
        u64::from(intended_range.max as u32),
    );
    before.insert(
        "active_id_i32_bits".to_owned(),
        u64::from(plan.active_bin_id as u32),
    );
    if kind.is_add() {
        before.insert(
            "requested_loyal_raw".to_owned(),
            METEORA_LIQUIDITY_TEST_LOYAL_RAW,
        );
        before.insert(
            "requested_usdc_raw".to_owned(),
            METEORA_LIQUIDITY_TEST_USDC_RAW,
        );
    } else {
        before.insert("requested_remove_bps".to_owned(), 10_000);
    }
    ensure_meteora_live_step(path, state, step_name, before)?;
    let before = meteora_live_step(state, step_name)?.before.clone();

    if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, step_name)? {
        let after = verify_meteora_liquidity_step(rpc, vault, &plan, kind, &before)?;
        return finalize_meteora_live_step(rpc, path, state, step_name, signature, after);
    }
    if meteora_live_step(state, step_name)?.status == PolicyStatus::Finalized {
        verify_meteora_liquidity_step(rpc, vault, &plan, kind, &before)?;
        println!("{step_name}=PASS already_finalized=true");
        return Ok(());
    }
    validate_meteora_liquidity_before(kind, &before)?;
    let range = meteora_range_from_observations(&before)?;
    let active_id = i32_observation(&before, "active_id_i32_bits")?;

    let (execute, policy_address) = build_meteora_liquidity_policy_execution(
        state,
        delegated.pubkey(),
        vault,
        &plan,
        kind,
        active_id,
        range,
    )?;
    let compute = ComputeBudgetInstruction::set_compute_unit_limit(600_000);
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &[compute, execute], delegated)?;
    let units = simulate_signed_transaction(rpc, &transaction, step_name)?;
    println!("{step_name}_simulation=PASS units_consumed={units}");
    println!("policy_execution_path={policy_address}");
    println!("policy_signer={}", delegated.pubkey());
    println!("settings_setup_path_used=false");
    println!("strategy_range={}..={}", range.min, range.max);
    let signature = send_meteora_live_transaction(
        rpc,
        path,
        state,
        step_name,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_meteora_liquidity_step(rpc, vault, &plan, kind, &before)?;
    finalize_meteora_live_step(rpc, path, state, step_name, signature, after)
}

fn simulate_meteora_fee_swap(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, MeteoraExecutionKind::AddB.step_name())?;
    let before = meteora_fee_swap_observations(rpc, deployment.pubkey(), vault, &plan)?;
    if observed(&before, "position_nonzero_liquidity_bins")? == 0 {
        bail!("direct Meteora fee swap requires active range-B liquidity");
    }
    let swap = meteora::build_direct_fee_swap(rpc, deployment.pubkey())?;
    if observed(&before, "deployment_usdc_raw")? < swap.amount_in_usdc_raw {
        bail!("deployment wallet has insufficient USDC for the direct Meteora fee swap");
    }
    let compute = ComputeBudgetInstruction::set_compute_unit_limit(1_400_000);
    let (transaction, _, _) =
        build_signed_transaction(rpc, &[compute, swap.instruction.clone()], deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, "meteora-generate-fees")?;
    println!("module=meteora-generate-fees simulation verdict=PASS");
    println!("swap_path=direct-meteora-dlmm deployment_controlled=true");
    println!("amount_in_usdc_raw={}", swap.amount_in_usdc_raw);
    println!("quoted_loyal_out_raw={}", swap.quoted_loyal_out_raw);
    println!("minimum_loyal_out_raw={}", swap.minimum_loyal_out_raw);
    println!("quoted_fee_raw={}", swap.quoted_fee_raw);
    println!("quoted_protocol_fee_raw={}", swap.quoted_protocol_fee_raw);
    for bin_array in &swap.bin_arrays {
        println!("swap_bin_array={bin_array}");
    }
    println!("units_consumed={units}");
    println!("transaction_sent=false");
    Ok(())
}

fn execute_meteora_fee_swap(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    const STEP: &str = "meteora-generate-fees";
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, MeteoraExecutionKind::AddB.step_name())?;
    let swap = meteora::build_direct_fee_swap(rpc, deployment.pubkey())?;
    let mut before = meteora_fee_swap_observations(rpc, deployment.pubkey(), vault, &plan)?;
    before.insert(
        "expected_usdc_input_raw".to_owned(),
        swap.amount_in_usdc_raw,
    );
    before.insert(
        "quoted_loyal_output_raw".to_owned(),
        swap.quoted_loyal_out_raw,
    );
    before.insert(
        "minimum_loyal_output_raw".to_owned(),
        swap.minimum_loyal_out_raw,
    );
    before.insert("quoted_fee_raw".to_owned(), swap.quoted_fee_raw);
    before.insert(
        "quoted_protocol_fee_raw".to_owned(),
        swap.quoted_protocol_fee_raw,
    );
    before.insert(
        "swap_bin_array_count".to_owned(),
        swap.bin_arrays.len() as u64,
    );
    ensure_meteora_live_step(path, state, STEP, before)?;
    let before = meteora_live_step(state, STEP)?.before.clone();

    if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, STEP)? {
        let after = verify_meteora_fee_swap(rpc, deployment.pubkey(), vault, &plan, &before)?;
        return finalize_meteora_live_step(rpc, path, state, STEP, signature, after);
    }
    if meteora_live_step(state, STEP)?.status == PolicyStatus::Finalized {
        verify_meteora_fee_swap(rpc, deployment.pubkey(), vault, &plan, &before)?;
        println!("{STEP}=PASS already_finalized=true");
        return Ok(());
    }
    if observed(&before, "position_nonzero_liquidity_bins")? == 0
        || observed(&before, "deployment_usdc_raw")? < swap.amount_in_usdc_raw
    {
        bail!("direct Meteora fee-swap prerequisites are not satisfied");
    }
    for (field, current) in [
        ("expected_usdc_input_raw", swap.amount_in_usdc_raw),
        ("quoted_loyal_output_raw", swap.quoted_loyal_out_raw),
        ("minimum_loyal_output_raw", swap.minimum_loyal_out_raw),
        ("quoted_fee_raw", swap.quoted_fee_raw),
        ("quoted_protocol_fee_raw", swap.quoted_protocol_fee_raw),
        ("swap_bin_array_count", swap.bin_arrays.len() as u64),
    ] {
        if observed(&before, field)? != current {
            bail!("fresh direct Meteora quote changed recorded field {field}; refusing retry");
        }
    }

    let compute = ComputeBudgetInstruction::set_compute_unit_limit(1_400_000);
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &[compute, swap.instruction], deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, STEP)?;
    println!("{STEP}_simulation=PASS units_consumed={units}");
    println!("swap_path=direct-meteora-dlmm deployment_controlled=true");
    println!("setup_or_policy_path_used=false");
    let signature = send_meteora_live_transaction(
        rpc,
        path,
        state,
        STEP,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_meteora_fee_swap(rpc, deployment.pubkey(), vault, &plan, &before)?;
    finalize_meteora_live_step(rpc, path, state, STEP, signature, after)
}

fn meteora_fee_swap_observations(
    rpc: &RpcClient,
    deployment: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
) -> Result<BTreeMap<String, u64>> {
    let mut observations = meteora_liquidity_observations(rpc, vault, plan)?;
    let deployment_loyal = derive_associated_token_address(
        deployment,
        loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    );
    let deployment_usdc = derive_associated_token_address(deployment, USDC_MINT);
    observations.insert(
        "deployment_loyal_raw".to_owned(),
        token_account_amount(
            rpc,
            deployment_loyal,
            deployment,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .context("deployment LOYAL account is absent")?,
    );
    observations.insert(
        "deployment_usdc_raw".to_owned(),
        token_account_amount(rpc, deployment_usdc, deployment, USDC_MINT)?
            .context("deployment USDC account is absent")?,
    );
    Ok(observations)
}

fn verify_meteora_fee_swap(
    rpc: &RpcClient,
    deployment: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = meteora_fee_swap_observations(rpc, deployment, vault, plan)?;
    let amount_in = observed(before, "expected_usdc_input_raw")?;
    let minimum_out = observed(before, "minimum_loyal_output_raw")?;
    let before_deployment_usdc = observed(before, "deployment_usdc_raw")?;
    let after_deployment_usdc = observed(&after, "deployment_usdc_raw")?;
    let before_deployment_loyal = observed(before, "deployment_loyal_raw")?;
    let after_deployment_loyal = observed(&after, "deployment_loyal_raw")?;
    let loyal_out = after_deployment_loyal
        .checked_sub(before_deployment_loyal)
        .context("direct Meteora fee swap unexpectedly reduced deployment LOYAL")?;
    if before_deployment_usdc.checked_sub(amount_in) != Some(after_deployment_usdc)
        || loyal_out < minimum_out
        || observed(before, "pool_usdc_reserve_raw")?.checked_add(amount_in)
            != Some(observed(&after, "pool_usdc_reserve_raw")?)
        || observed(&after, "pool_loyal_reserve_raw")?.checked_add(loyal_out)
            != Some(observed(before, "pool_loyal_reserve_raw")?)
        || observed(&after, "position_nonzero_liquidity_bins")? == 0
        || before.get("position_lamports") != after.get("position_lamports")
        || before.get("position_data_len") != after.get("position_data_len")
        || before.get("vault_lamports") != after.get("vault_lamports")
        || before.get("vault_loyal_raw") != after.get("vault_loyal_raw")
        || before.get("vault_usdc_raw") != after.get("vault_usdc_raw")
    {
        bail!("direct Meteora fee swap does not match the quoted deployment-controlled manifest");
    }
    for index in 0..plan.bin_arrays.len() {
        if after.get(&format!("bin_array_{index}_exists")) != Some(&1) {
            bail!("required Meteora BinArray {index} did not persist through direct swap");
        }
    }
    Ok(after)
}

fn simulate_meteora_claim_fees(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, MeteoraExecutionKind::RemoveB.step_name())?;
    let before = meteora_liquidity_observations(rpc, vault, &plan)?;
    validate_meteora_claim_before(&before)?;
    let chunks = meteora::position_range_chunks(&plan)?;
    for (index, range) in chunks.iter().copied().enumerate() {
        let (execute, policy_address) =
            build_meteora_claim_policy_execution(state, delegated.pubkey(), vault, &plan, range)?;
        let compute = ComputeBudgetInstruction::set_compute_unit_limit(600_000);
        let (transaction, _, _) = build_signed_transaction(rpc, &[compute, execute], delegated)?;
        let label = format!("meteora-claim-fees-chunk-{index}");
        let units = simulate_signed_transaction(rpc, &transaction, &label)?;
        println!(
            "claim_chunk={index} range={}..={} policy={} units_consumed={units}",
            range.min, range.max, policy_address
        );
    }
    println!(
        "module=meteora-claim-fees simulation verdict=PASS chunks={}",
        chunks.len()
    );
    println!("policy_signer={}", delegated.pubkey());
    println!("transaction_sent=false");
    Ok(())
}

fn execute_meteora_claim_fees(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (_, vault, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    require_finalized_meteora_policies(state)?;
    require_finalized_meteora_live_step(state, MeteoraExecutionKind::RemoveB.step_name())?;
    let initial = meteora_liquidity_observations(rpc, vault, &plan)?;
    if observed(&initial, "position_nonzero_liquidity_bins")? != 0 {
        bail!("Meteora fee claim requires an empty position");
    }
    let pending_cycle = pending_meteora_claim_cycle(state)?;
    if observed(&initial, "position_pending_fee_loyal_raw")?
        .checked_add(observed(&initial, "position_pending_fee_usdc_raw")?)
        .unwrap_or(0)
        == 0
        && pending_cycle.is_none()
    {
        println!("meteora-claim-fees=PASS already_zero=true");
        return Ok(());
    }
    let cycle =
        pending_cycle.unwrap_or(rpc.get_slot_with_commitment(CommitmentConfig::finalized())?);
    let chunks = meteora::position_range_chunks(&plan)?;
    for (index, range) in chunks.iter().copied().enumerate() {
        let step_name = format!("meteora-claim-fees-cycle-{cycle}-chunk-{index}");
        let mut before = meteora_liquidity_observations(rpc, vault, &plan)?;
        before.insert("range_min_i32_bits".to_owned(), u64::from(range.min as u32));
        before.insert("range_max_i32_bits".to_owned(), u64::from(range.max as u32));
        ensure_meteora_live_step(path, state, &step_name, before)?;
        let before = meteora_live_step(state, &step_name)?.before.clone();
        let require_all_zero = index + 1 == chunks.len();

        if let Some(signature) = recover_finalized_meteora_live_step(rpc, state, &step_name)? {
            let after = verify_meteora_claim_chunk(rpc, vault, &plan, &before, require_all_zero)?;
            finalize_meteora_live_step(rpc, path, state, &step_name, signature, after)?;
            continue;
        }
        if meteora_live_step(state, &step_name)?.status == PolicyStatus::Finalized {
            println!("{step_name}=PASS already_finalized=true");
            continue;
        }
        let recorded_range = meteora_range_from_observations(&before)?;
        if recorded_range != range {
            bail!("recorded Meteora fee-claim chunk changed range");
        }
        let (execute, policy_address) =
            build_meteora_claim_policy_execution(state, delegated.pubkey(), vault, &plan, range)?;
        let compute = ComputeBudgetInstruction::set_compute_unit_limit(600_000);
        let (transaction, blockhash, last_valid_block_height) =
            build_signed_transaction(rpc, &[compute, execute], delegated)?;
        let units = simulate_signed_transaction(rpc, &transaction, &step_name)?;
        println!("{step_name}_simulation=PASS units_consumed={units}");
        println!("policy_execution_path={policy_address}");
        println!("claim_range={}..={}", range.min, range.max);
        let signature = send_meteora_live_transaction(
            rpc,
            path,
            state,
            &step_name,
            transaction,
            blockhash,
            last_valid_block_height,
        )?;
        let after = verify_meteora_claim_chunk(rpc, vault, &plan, &before, require_all_zero)?;
        finalize_meteora_live_step(rpc, path, state, &step_name, signature, after)?;
    }
    println!(
        "meteora-claim-fees=PASS cycle={cycle} chunks={}",
        chunks.len()
    );
    Ok(())
}

fn pending_meteora_claim_cycle(state: &VaultState) -> Result<Option<u64>> {
    const PREFIX: &str = "meteora-claim-fees-cycle-";
    let mut cycles = BTreeSet::new();
    for step in &state
        .meteora
        .as_ref()
        .context("Meteora state is missing")?
        .live_steps
    {
        if step.status != PolicyStatus::Planned {
            continue;
        }
        let Some(suffix) = step.name.strip_prefix(PREFIX) else {
            continue;
        };
        let cycle = suffix
            .split_once("-chunk-")
            .context("malformed pending Meteora claim-cycle step name")?
            .0
            .parse::<u64>()
            .context("parse pending Meteora claim-cycle slot")?;
        cycles.insert(cycle);
    }
    if cycles.len() > 1 {
        bail!("multiple incomplete Meteora fee-claim cycles require operator review");
    }
    Ok(cycles.into_iter().next())
}

fn build_meteora_claim_policy_execution(
    state: &VaultState,
    delegated: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    range: meteora::BinRange,
) -> Result<(Instruction, Pubkey)> {
    let inner = meteora::claim_fees_instruction(vault, plan, range)?;
    let record =
        meteora_policy_record_for_range(state, meteora::MeteoraPolicyKind::ClaimFees, range)?;
    let policy_address = Pubkey::from_str(&record.policy)?;
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner);
    let execute = execute_program_interaction_policy_instruction(
        policy_address,
        delegated,
        VAULT_INDEX,
        vec![compiled],
        vec![0],
        transaction_accounts,
    );
    Ok((execute, policy_address))
}

fn validate_meteora_claim_before(before: &BTreeMap<String, u64>) -> Result<()> {
    if observed(before, "position_nonzero_liquidity_bins")? != 0
        || observed(before, "position_pending_fee_loyal_raw")?
            .checked_add(observed(before, "position_pending_fee_usdc_raw")?)
            .unwrap_or(0)
            == 0
    {
        bail!("Meteora fee claim requires an empty position with nonzero settled fees");
    }
    Ok(())
}

fn verify_meteora_claim_chunk(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    before: &BTreeMap<String, u64>,
    require_all_zero: bool,
) -> Result<BTreeMap<String, u64>> {
    let after = meteora_liquidity_observations(rpc, vault, plan)?;
    let before_loyal = observed(before, "vault_loyal_raw")?;
    let after_loyal = observed(&after, "vault_loyal_raw")?;
    let before_usdc = observed(before, "vault_usdc_raw")?;
    let after_usdc = observed(&after, "vault_usdc_raw")?;
    let loyal_delta = after_loyal
        .checked_sub(before_loyal)
        .context("Meteora fee claim unexpectedly reduced vault LOYAL")?;
    let usdc_delta = after_usdc
        .checked_sub(before_usdc)
        .context("Meteora fee claim unexpectedly reduced vault USDC")?;
    let before_pending_loyal = observed(before, "position_pending_fee_loyal_raw")?;
    let after_pending_loyal = observed(&after, "position_pending_fee_loyal_raw")?;
    let before_pending_usdc = observed(before, "position_pending_fee_usdc_raw")?;
    let after_pending_usdc = observed(&after, "position_pending_fee_usdc_raw")?;
    if before_pending_loyal.checked_sub(after_pending_loyal) != Some(loyal_delta)
        || before_pending_usdc.checked_sub(after_pending_usdc) != Some(usdc_delta)
        || (require_all_zero && (after_pending_loyal != 0 || after_pending_usdc != 0))
        || observed(&after, "position_nonzero_liquidity_bins")? != 0
        || observed(&after, "pool_loyal_reserve_raw")?.checked_add(loyal_delta)
            != Some(observed(before, "pool_loyal_reserve_raw")?)
        || observed(&after, "pool_usdc_reserve_raw")?.checked_add(usdc_delta)
            != Some(observed(before, "pool_usdc_reserve_raw")?)
        || before.get("vault_lamports") != after.get("vault_lamports")
        || before.get("position_lamports") != after.get("position_lamports")
        || before.get("position_data_len") != after.get("position_data_len")
        || after.get("position_exists") != Some(&1)
    {
        bail!("delegated Meteora fee claim does not match the exact persistence manifest");
    }
    for index in 0..plan.bin_arrays.len() {
        if after.get(&format!("bin_array_{index}_exists")) != Some(&1) {
            bail!("required Meteora BinArray {index} did not persist through fee claim");
        }
    }
    Ok(after)
}

fn build_meteora_liquidity_policy_execution(
    state: &VaultState,
    delegated: Pubkey,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    kind: MeteoraExecutionKind,
    active_id: i32,
    range: meteora::BinRange,
) -> Result<(Instruction, Pubkey)> {
    let inner = if kind.is_add() {
        meteora::add_liquidity_instruction(
            vault,
            plan,
            METEORA_LIQUIDITY_TEST_LOYAL_RAW,
            METEORA_LIQUIDITY_TEST_USDC_RAW,
            active_id,
            range,
        )?
    } else {
        meteora::remove_liquidity_instruction(vault, plan, range, 10_000)?
    };
    let record = meteora_policy_record_for_range(state, kind.policy_kind(), range)?;
    if record.status != PolicyStatus::Finalized {
        bail!("Meteora execution policy is not finalized");
    }
    let policy_address = Pubkey::from_str(&record.policy)?;
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner);
    Ok((
        execute_program_interaction_policy_instruction(
            policy_address,
            delegated,
            VAULT_INDEX,
            vec![compiled],
            vec![0],
            transaction_accounts,
        ),
        policy_address,
    ))
}

fn meteora_policy_record_for_range(
    state: &VaultState,
    kind: meteora::MeteoraPolicyKind,
    range: meteora::BinRange,
) -> Result<&PolicyRecord> {
    let record = state
        .meteora
        .as_ref()
        .context("Meteora state record is missing")?;
    let lower_index = meteora::bin_array_index(range.min);
    let upper_index = meteora::bin_array_index(range.max);
    if upper_index < lower_index || upper_index > lower_index + 1 {
        bail!(
            "Meteora execution range {}..={} spans more than one two-BinArray policy window",
            range.min,
            range.max
        );
    }
    let selected = if [-4, -3].contains(&lower_index) {
        meteora::policy_record(record, kind)
    } else {
        record
            .additional_policy_shards
            .iter()
            .find(|shard| shard.lower_bin_array_indexes.contains(&lower_index))
            .and_then(|shard| match kind {
                meteora::MeteoraPolicyKind::AddLiquidity => shard.add_liquidity_policy.as_ref(),
                meteora::MeteoraPolicyKind::RemoveLiquidity => {
                    shard.remove_liquidity_policy.as_ref()
                }
                meteora::MeteoraPolicyKind::ClaimFees => shard.claim_fee_policy.as_ref(),
            })
    }
    .with_context(|| {
        format!(
            "no finalized Meteora {} policy covers lower BinArray index {lower_index}",
            kind.label()
        )
    })?;
    if selected.status != PolicyStatus::Finalized {
        bail!("selected Meteora {} policy is not finalized", kind.label());
    }
    Ok(selected)
}

fn meteora_range_from_observations(
    observations: &BTreeMap<String, u64>,
) -> Result<meteora::BinRange> {
    Ok(meteora::BinRange {
        min: i32_observation(observations, "range_min_i32_bits")?,
        max: i32_observation(observations, "range_max_i32_bits")?,
    })
}

fn i32_observation(observations: &BTreeMap<String, u64>, field: &str) -> Result<i32> {
    let bits = observed(observations, field)?;
    let bits = u32::try_from(bits).with_context(|| format!("{field} exceeds an encoded i32"))?;
    Ok(i32::from_ne_bytes(bits.to_ne_bytes()))
}

fn meteora_liquidity_observations(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
) -> Result<BTreeMap<String, u64>> {
    let position = meteora::load_position_snapshot(rpc, plan.position, vault)?
        .context("approved Meteora position is absent")?;
    let mut observations = BTreeMap::new();
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    observations.insert(
        "vault_loyal_raw".to_owned(),
        token_account_amount(
            rpc,
            plan.vault_loyal,
            vault,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .context("vault LOYAL account is absent")?,
    );
    observations.insert(
        "vault_usdc_raw".to_owned(),
        token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?
            .context("vault USDC account is absent")?,
    );
    observations.insert(
        "pool_loyal_reserve_raw".to_owned(),
        token_account_amount(
            rpc,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_RESERVE,
            loyal_actions::autonomous_vaults::METEORA_POOL,
            loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
        )?
        .context("Meteora LOYAL reserve is absent")?,
    );
    observations.insert(
        "pool_usdc_reserve_raw".to_owned(),
        token_account_amount(
            rpc,
            loyal_actions::autonomous_vaults::METEORA_USDC_RESERVE,
            loyal_actions::autonomous_vaults::METEORA_POOL,
            USDC_MINT,
        )?
        .context("Meteora USDC reserve is absent")?,
    );
    observations.insert("position_exists".to_owned(), 1);
    observations.insert("position_lamports".to_owned(), position.lamports);
    observations.insert("position_data_len".to_owned(), position.data_len as u64);
    observations.insert(
        "position_nonzero_liquidity_bins".to_owned(),
        position.nonzero_liquidity_bins,
    );
    observations.insert(
        "position_pending_fee_loyal_raw".to_owned(),
        position.pending_fee_x,
    );
    observations.insert(
        "position_pending_fee_usdc_raw".to_owned(),
        position.pending_fee_y,
    );
    for (index, bin_array) in plan.bin_arrays.iter().enumerate() {
        observations.insert(
            format!("bin_array_{index}_exists"),
            u64::from(account_exists_with_owner(
                rpc,
                *bin_array,
                loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID,
            )?),
        );
    }
    Ok(observations)
}

fn validate_meteora_liquidity_before(
    kind: MeteoraExecutionKind,
    before: &BTreeMap<String, u64>,
) -> Result<()> {
    if before.get("position_exists") != Some(&1) {
        bail!("approved Meteora position must exist before delegated execution");
    }
    let bins = before
        .get("position_nonzero_liquidity_bins")
        .copied()
        .context("missing Meteora position liquidity observation")?;
    if kind.is_add() {
        if bins != 0 {
            bail!("Meteora add canary requires an empty persistent position");
        }
        if before.get("vault_loyal_raw").copied().unwrap_or(0) < METEORA_LIQUIDITY_TEST_LOYAL_RAW
            || before.get("vault_usdc_raw").copied().unwrap_or(0) < METEORA_LIQUIDITY_TEST_USDC_RAW
        {
            bail!("vault has insufficient LOYAL or USDC dust for the Meteora add canary");
        }
    } else if bins == 0 {
        bail!("Meteora remove canary requires nonzero position liquidity");
    }
    Ok(())
}

fn verify_meteora_liquidity_step(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &meteora::MeteoraPlan,
    kind: MeteoraExecutionKind,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = meteora_liquidity_observations(rpc, vault, plan)?;
    for index in 0..plan.bin_arrays.len() {
        if after.get(&format!("bin_array_{index}_exists")) != Some(&1) {
            bail!("required Meteora BinArray {index} did not persist");
        }
    }
    for field in ["vault_lamports", "position_lamports", "position_data_len"] {
        if before.get(field) != after.get(field) {
            bail!("Meteora delegated execution changed persistent account field {field}");
        }
    }
    if after.get("position_exists") != Some(&1) {
        bail!("Meteora delegated execution removed the approved position");
    }
    let before_loyal = observed(before, "vault_loyal_raw")?;
    let after_loyal = observed(&after, "vault_loyal_raw")?;
    let before_usdc = observed(before, "vault_usdc_raw")?;
    let after_usdc = observed(&after, "vault_usdc_raw")?;
    let before_pool_loyal = observed(before, "pool_loyal_reserve_raw")?;
    let after_pool_loyal = observed(&after, "pool_loyal_reserve_raw")?;
    let before_pool_usdc = observed(before, "pool_usdc_reserve_raw")?;
    let after_pool_usdc = observed(&after, "pool_usdc_reserve_raw")?;

    if kind.is_add() {
        let loyal_delta = before_loyal
            .checked_sub(after_loyal)
            .context("Meteora add unexpectedly increased vault LOYAL")?;
        let usdc_delta = before_usdc
            .checked_sub(after_usdc)
            .context("Meteora add unexpectedly increased vault USDC")?;
        if (loyal_delta == 0 && usdc_delta == 0)
            || loyal_delta > METEORA_LIQUIDITY_TEST_LOYAL_RAW
            || usdc_delta > METEORA_LIQUIDITY_TEST_USDC_RAW
            || before_pool_loyal.checked_add(loyal_delta) != Some(after_pool_loyal)
            || before_pool_usdc.checked_add(usdc_delta) != Some(after_pool_usdc)
            || observed(&after, "position_nonzero_liquidity_bins")? == 0
            || before.get("position_pending_fee_loyal_raw")
                != after.get("position_pending_fee_loyal_raw")
            || before.get("position_pending_fee_usdc_raw")
                != after.get("position_pending_fee_usdc_raw")
        {
            bail!("delegated Meteora add does not match the exact principal and position manifest");
        }
    } else {
        let loyal_delta = after_loyal
            .checked_sub(before_loyal)
            .context("Meteora remove unexpectedly reduced vault LOYAL")?;
        let usdc_delta = after_usdc
            .checked_sub(before_usdc)
            .context("Meteora remove unexpectedly reduced vault USDC")?;
        let before_fee_loyal = observed(before, "position_pending_fee_loyal_raw")?;
        let after_fee_loyal = observed(&after, "position_pending_fee_loyal_raw")?;
        let before_fee_usdc = observed(before, "position_pending_fee_usdc_raw")?;
        let after_fee_usdc = observed(&after, "position_pending_fee_usdc_raw")?;
        if (loyal_delta == 0 && usdc_delta == 0)
            || after_pool_loyal.checked_add(loyal_delta) != Some(before_pool_loyal)
            || after_pool_usdc.checked_add(usdc_delta) != Some(before_pool_usdc)
            || observed(&after, "position_nonzero_liquidity_bins")? != 0
            || after_fee_loyal < before_fee_loyal
            || after_fee_usdc < before_fee_usdc
            || (kind == MeteoraExecutionKind::RemoveB
                && after_fee_loyal.checked_add(after_fee_usdc).unwrap_or(0) == 0)
        {
            bail!(
                "delegated Meteora remove does not match the exact principal and persistence manifest"
            );
        }
    }
    Ok(after)
}

fn observed(observations: &BTreeMap<String, u64>, field: &str) -> Result<u64> {
    observations
        .get(field)
        .copied()
        .with_context(|| format!("missing Meteora observation {field}"))
}

fn require_finalized_meteora_policies(state: &VaultState) -> Result<()> {
    let record = state
        .meteora
        .as_ref()
        .context("Meteora state record is missing")?;
    for (kind, label) in [
        (meteora::MeteoraPolicyKind::AddLiquidity, "add"),
        (meteora::MeteoraPolicyKind::RemoveLiquidity, "remove"),
        (meteora::MeteoraPolicyKind::ClaimFees, "claim"),
    ] {
        if meteora::policy_record(record, kind).map(|policy| policy.status)
            != Some(PolicyStatus::Finalized)
        {
            bail!("Meteora {label} policy must be finalized before delegated execution");
        }
    }
    if record.policy_generation == meteora::METEORA_EXPANDED_POLICY_GENERATION {
        if record.additional_policy_shards.len() != 2 {
            bail!("expanded Meteora policy manifest must contain exactly two additional shards");
        }
        for shard in &record.additional_policy_shards {
            for (policy, label) in [
                (shard.add_liquidity_policy.as_ref(), "add"),
                (shard.remove_liquidity_policy.as_ref(), "remove"),
                (shard.claim_fee_policy.as_ref(), "claim"),
            ] {
                if policy.map(|policy| policy.status) != Some(PolicyStatus::Finalized) {
                    bail!(
                        "Meteora shard {} {label} policy must be finalized before delegated execution",
                        shard.shard_index
                    );
                }
            }
        }
    }
    Ok(())
}

fn optional_account_lamports(rpc: &RpcClient, address: Pubkey) -> Result<u64> {
    Ok(rpc
        .get_account_with_commitment(&address, CommitmentConfig::finalized())?
        .value
        .map(|account| account.lamports)
        .unwrap_or(0))
}

fn send_meteora_live_transaction(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    transaction: Transaction,
    blockhash: solana_sdk::hash::Hash,
    last_valid_block_height: u64,
) -> Result<Signature> {
    let pending_signature = transaction.signatures[0];
    {
        let step = meteora_live_step_mut(state, name)?;
        step.pending_signature = Some(pending_signature.to_string());
        step.last_valid_block_height = Some(last_valid_block_height);
    }
    state::save(path, state)?;
    let sent = rpc.send_transaction_with_config(
        &transaction,
        RpcSendTransactionConfig {
            skip_preflight: false,
            preflight_commitment: Some(CommitmentLevel::Finalized),
            ..RpcSendTransactionConfig::default()
        },
    )?;
    if sent != pending_signature {
        bail!("RPC returned a different transaction signature for {name}");
    }
    rpc.confirm_transaction_with_spinner(&sent, &blockhash, CommitmentConfig::finalized())?;
    Ok(sent)
}

fn require_finalized_meteora_live_step(state: &VaultState, name: &str) -> Result<()> {
    if meteora_live_step(state, name)?.status != PolicyStatus::Finalized {
        bail!("Meteora prerequisite {name} is not finalized");
    }
    Ok(())
}

fn simulate_meteora_policy(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: meteora::MeteoraPolicyKind,
) -> Result<()> {
    let (settings, _, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    enforce_meteora_policy_prerequisites(state, kind)?;
    let policy_plan = meteora_policy_plan(&plan, kind).0;
    if rpc.get_account(&policy_plan.policy).is_ok() {
        bail!("{} policy already exists; inspect it instead", kind.label());
    }
    verify_next_policy_seed(rpc, settings, kind.seed())?;
    let (transaction, _, _) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    println!("module={} policy-simulation verdict=PASS", kind.label());
    println!("policy={}", policy_plan.policy);
    println!("policy_seed={}", policy_plan.policy_seed);
    println!("units_consumed={units}");
    println!("transaction_sent=false");
    Ok(())
}

fn create_or_resume_meteora_policy(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: meteora::MeteoraPolicyKind,
) -> Result<()> {
    let (settings, _, plan) = load_meteora_plan(rpc, state, deployment, delegated)?;
    ensure_meteora_record(path, state, &plan)?;
    enforce_meteora_policy_prerequisites(state, kind)?;
    let policy_plan = meteora_policy_plan(&plan, kind).0;
    if meteora::policy_record(
        state
            .meteora
            .as_ref()
            .context("Meteora record is missing")?,
        kind,
    )
    .is_none()
    {
        *meteora::policy_record_mut(
            state
                .meteora
                .as_mut()
                .context("Meteora record is missing")?,
            kind,
        ) = Some(PolicyRecord {
            status: PolicyStatus::Planned,
            seed: kind.seed(),
            policy: policy_plan.policy.to_string(),
            pending_signature: None,
            last_valid_block_height: None,
            creation_signature: None,
            finalized_slot: None,
        });
        state::save(path, state)?;
    }
    let record = meteora::policy_record(
        state
            .meteora
            .as_ref()
            .context("Meteora record is missing")?,
        kind,
    )
    .context("Meteora policy record is missing")?;
    if record.seed != kind.seed() || record.policy != policy_plan.policy.to_string() {
        bail!(
            "recorded {} policy identity does not match the fresh plan",
            kind.label()
        );
    }
    if record.status == PolicyStatus::Finalized {
        verify_meteora_policy_account(
            rpc,
            &plan,
            kind,
            settings,
            deployment.pubkey(),
            delegated.pubkey(),
        )?;
        println!("{} policy is already finalized", kind.label());
        return Ok(());
    }
    if let Some(signature) = record.pending_signature.as_deref() {
        let signature = Signature::from_str(signature)?;
        if let Some(status) = rpc
            .get_signature_statuses(&[signature])?
            .value
            .into_iter()
            .next()
            .flatten()
        {
            if let Some(error) = status.err {
                bail!("recorded {} creation failed: {error:?}", kind.label());
            }
            if status.satisfies_commitment(CommitmentConfig::finalized()) {
                return finalize_meteora_policy_record(
                    rpc,
                    path,
                    state,
                    &plan,
                    kind,
                    settings,
                    deployment.pubkey(),
                    delegated.pubkey(),
                    signature,
                );
            }
            bail!("recorded {} creation is not finalized yet", kind.label());
        }
        let last_valid = record
            .last_valid_block_height
            .context("pending policy is missing last valid block height")?;
        if rpc.get_block_height()? <= last_valid {
            bail!("recorded policy signature is still live but not visible");
        }
    }
    if rpc.get_account(&policy_plan.policy).is_ok() {
        bail!("planned Meteora policy exists without recoverable finalized evidence");
    }
    verify_next_policy_seed(rpc, settings, kind.seed())?;
    let (transaction, blockhash, last_valid_block_height) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    println!("{}_simulation=PASS units_consumed={units}", kind.label());
    let pending = transaction.signatures[0];
    {
        let record = meteora::policy_record_mut(
            state
                .meteora
                .as_mut()
                .context("Meteora record is missing")?,
            kind,
        )
        .as_mut()
        .context("Meteora policy record is missing")?;
        record.pending_signature = Some(pending.to_string());
        record.last_valid_block_height = Some(last_valid_block_height);
    }
    state::save(path, state)?;
    let sent = rpc.send_transaction_with_config(
        &transaction,
        RpcSendTransactionConfig {
            skip_preflight: false,
            preflight_commitment: Some(CommitmentLevel::Finalized),
            ..RpcSendTransactionConfig::default()
        },
    )?;
    if sent != pending {
        bail!("RPC returned a different Meteora policy signature");
    }
    rpc.confirm_transaction_with_spinner(&sent, &blockhash, CommitmentConfig::finalized())?;
    finalize_meteora_policy_record(
        rpc,
        path,
        state,
        &plan,
        kind,
        settings,
        deployment.pubkey(),
        delegated.pubkey(),
        sent,
    )
}

fn enforce_meteora_policy_prerequisites(
    state: &VaultState,
    kind: meteora::MeteoraPolicyKind,
) -> Result<()> {
    require_finalized_meteora_live_step(state, "meteora-account-setup")?;
    let record = state
        .meteora
        .as_ref()
        .context("Meteora record is missing")?;
    match kind {
        meteora::MeteoraPolicyKind::AddLiquidity => Ok(()),
        meteora::MeteoraPolicyKind::RemoveLiquidity => {
            if record
                .add_liquidity_policy
                .as_ref()
                .map(|policy| policy.status)
                != Some(PolicyStatus::Finalized)
            {
                bail!("Meteora add policy must finalize before the remove policy");
            }
            Ok(())
        }
        meteora::MeteoraPolicyKind::ClaimFees => {
            if record
                .remove_liquidity_policy
                .as_ref()
                .map(|policy| policy.status)
                != Some(PolicyStatus::Finalized)
            {
                bail!("Meteora remove policy must finalize before the claim policy");
            }
            Ok(())
        }
    }
}

fn verify_meteora_policy_account(
    rpc: &RpcClient,
    plan: &meteora::MeteoraPlan,
    kind: meteora::MeteoraPolicyKind,
    settings: Pubkey,
    deployment: Pubkey,
    delegated: Pubkey,
) -> Result<policy::ProgramInteractionPolicyAccount> {
    let (policy_plan, constraints) = meteora_policy_plan(plan, kind);
    verify_meteora_policy_plan_account(
        rpc,
        policy_plan,
        constraints,
        settings,
        deployment,
        delegated,
        kind.label(),
    )
}

fn verify_all_meteora_policy_accounts(
    rpc: &RpcClient,
    plan: &meteora::MeteoraPlan,
    settings: Pubkey,
    deployment: Pubkey,
    delegated: Pubkey,
) -> Result<()> {
    for kind in [
        meteora::MeteoraPolicyKind::AddLiquidity,
        meteora::MeteoraPolicyKind::RemoveLiquidity,
        meteora::MeteoraPolicyKind::ClaimFees,
    ] {
        verify_meteora_policy_account(rpc, plan, kind, settings, deployment, delegated)?;
    }
    for shard in &plan.additional_policy_shards {
        for kind in [
            meteora::MeteoraPolicyKind::AddLiquidity,
            meteora::MeteoraPolicyKind::RemoveLiquidity,
            meteora::MeteoraPolicyKind::ClaimFees,
        ] {
            let (policy_plan, constraints) = meteora_shard_policy_plan(shard, kind);
            verify_meteora_policy_plan_account(
                rpc,
                policy_plan,
                constraints,
                settings,
                deployment,
                delegated,
                &meteora_upgrade_step_name(shard.shard_index, kind),
            )?;
        }
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn verify_meteora_policy_plan_account(
    rpc: &RpcClient,
    policy_plan: &loyal_actions::autonomous_vaults::MeteoraPolicyPlan,
    constraints: &[loyal_actions::SquadsInstructionConstraintView],
    settings: Pubkey,
    deployment: Pubkey,
    delegated: Pubkey,
    label: &str,
) -> Result<policy::ProgramInteractionPolicyAccount> {
    let account = rpc
        .get_account(&policy_plan.policy)
        .with_context(|| format!("reload {label} policy"))?;
    let decoded = policy::decode_program_interaction_policy(account.owner, &account.data)?;
    policy::verify_program_interaction_policy(
        &decoded,
        policy::ExpectedProgramInteractionPolicy {
            policy_address: policy_plan.policy,
            settings,
            seed: policy_plan.policy_seed,
            delegated_signer: delegated,
            account_index: VAULT_INDEX,
            constraints,
            rent_collector: deployment,
        },
    )?;
    Ok(decoded)
}

#[allow(clippy::too_many_arguments)]
fn finalize_meteora_policy_record(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    plan: &meteora::MeteoraPlan,
    kind: meteora::MeteoraPolicyKind,
    settings: Pubkey,
    deployment: Pubkey,
    delegated: Pubkey,
    signature: Signature,
) -> Result<()> {
    let decoded = verify_meteora_policy_account(rpc, plan, kind, settings, deployment, delegated)?;
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let record = meteora::policy_record_mut(
        state
            .meteora
            .as_mut()
            .context("Meteora record is missing")?,
        kind,
    )
    .as_mut()
    .context("Meteora policy record is missing")?;
    record.status = PolicyStatus::Finalized;
    record.creation_signature = Some(signature.to_string());
    record.finalized_slot = Some(transaction.slot);
    state::save(path, state)?;
    println!(
        "create_{}_policy=PASS policy={} signature={} slot={} start={} rent_collector={}",
        kind.label(),
        meteora_policy_plan(plan, kind).0.policy,
        signature,
        transaction.slot,
        decoded.start,
        decoded.rent_collector
    );
    Ok(())
}

fn inspect_returns(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let plan = returns::load_plan(rpc, state, deployment.pubkey(), delegated.pubkey())?;
    if let Some(record) = &state.returns {
        returns::validate_record(record, &plan)?;
    }

    println!("module=treasury-returns-readiness verdict=PASS");
    println!("settings={}", plan.settings);
    println!("vault={}", plan.vault);
    println!(
        "mother={}",
        loyal_actions::autonomous_vaults::MOTHER_TREASURY_VAULT
    );
    for kind in [TreasuryReturnKind::Loyal, TreasuryReturnKind::Usdc] {
        let policy = plan.policy(kind);
        let vault_amount =
            token_account_amount(rpc, policy.source_token_account, plan.vault, policy.mint)?
                .context("validated vault return ATA disappeared")?;
        let mother_amount = token_account_amount(
            rpc,
            policy.destination_token_account,
            policy.destination_owner,
            policy.mint,
        )?
        .context("validated Mother return ATA disappeared")?;
        println!("{}_mint={}", kind.label().to_lowercase(), policy.mint);
        println!(
            "{}_vault_token_account={}",
            kind.label().to_lowercase(),
            policy.source_token_account
        );
        println!(
            "{}_mother_token_account={}",
            kind.label().to_lowercase(),
            policy.destination_token_account
        );
        println!("{}_vault_raw={vault_amount}", kind.label().to_lowercase());
        println!("{}_mother_raw={mother_amount}", kind.label().to_lowercase());

        let recorded = state
            .returns
            .as_ref()
            .and_then(|record| returns::policy_record(record, kind));
        if recorded.map(|record| record.status) == Some(PolicyStatus::Finalized) {
            let decoded = returns::decode_and_verify_policy(
                rpc,
                &plan,
                kind,
                delegated.pubkey(),
                deployment.pubkey(),
                expected_return_allowance(state, &plan, kind),
            )?;
            println!("{}_policy={}", kind.label().to_lowercase(), policy.policy);
            println!(
                "{}_policy_remaining={}",
                kind.label().to_lowercase(),
                decoded.remaining_in_period
            );
            println!("{}_policy_verdict=PASS", kind.label().to_lowercase());
        } else {
            println!("{}_policy=PENDING", kind.label().to_lowercase());
        }
    }
    Ok(())
}

fn verify_all(
    rpc: &RpcClient,
    path: &std::path::Path,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    inspect(rpc, path, Some(state), deployment, delegated)?;
    let smart = state
        .smart_account
        .as_ref()
        .context("Smart Account record is missing")?;
    if smart.status != SmartAccountStatus::Finalized {
        bail!("Smart Account is not finalized");
    }
    let settings = Pubkey::from_str(&smart.settings)?;
    let vault = Pubkey::from_str(&smart.vault)?;
    let settings_account = rpc.get_account(&settings)?;
    let decoded_settings = squads::decode_settings(&settings_account.data)?;
    let expected_policy_seed = if state
        .meteora
        .as_ref()
        .context("Meteora state is missing")?
        .policy_generation
        == meteora::METEORA_EXPANDED_POLICY_GENERATION
    {
        13
    } else {
        7
    };
    if decoded_settings.policy_seed != Some(expected_policy_seed) {
        bail!("Settings policy seed does not match the recorded policy generation");
    }
    println!("module=smart-account verdict=PASS policy_seed={expected_policy_seed}");

    let (kamino_settings, kamino_vault, kamino_plan) =
        load_kamino_plan(rpc, state, deployment, delegated)?;
    for kind in [
        KaminoPolicyKind::Operations,
        KaminoPolicyKind::InitObligation,
    ] {
        verify_policy_account(
            rpc,
            &kamino_plan,
            kind,
            kamino_settings,
            deployment.pubkey(),
            delegated.pubkey(),
        )?;
    }
    if !account_exists_with_owner(
        rpc,
        derive_kamino_user_metadata(kamino_vault),
        KAMINO_LEND_PROGRAM_ID,
    )? {
        bail!("Kamino UserMetadata is absent during final verification");
    }
    for (index, reserve) in kamino_plan.reserves.iter().enumerate() {
        let obligation = kamino::load_obligation_snapshot(rpc, kamino_vault, reserve)?;
        if !obligation.exists
            || obligation.deposited_amount != 0
            || obligation.lamports != 24_165_120
        {
            bail!("Kamino obligation {index} is not the recorded persistent zero state");
        }
        let farm = reserve
            .obligation_farm_user_state
            .context("approved Kamino reserve has no farm-user address")?;
        if !account_exists_with_owner(rpc, farm, KAMINO_FARMS_PROGRAM_ID)? {
            bail!("Kamino farm user {index} is absent during final verification");
        }
    }
    require_all_steps_finalized(
        "kamino",
        &state
            .kamino
            .as_ref()
            .context("Kamino state is missing")?
            .live_steps,
        12,
    )?;
    println!("module=kamino verdict=PASS policies=2 obligations=2 farms=2");

    let (meteora_settings, meteora_vault, meteora_plan) =
        load_meteora_plan(rpc, state, deployment, delegated)?;
    verify_all_meteora_policy_accounts(
        rpc,
        &meteora_plan,
        meteora_settings,
        deployment.pubkey(),
        delegated.pubkey(),
    )?;
    let position = meteora::load_position_snapshot(rpc, meteora_plan.position, meteora_vault)?
        .context("Meteora position is absent during final verification")?;
    if position.lower_bin_id != meteora::POSITION_LOWER_BIN_ID
        || position.upper_bin_id != meteora::POSITION_TARGET_UPPER_BIN_ID
        || position.nonzero_liquidity_bins != 0
        || position.pending_fee_x != 0
        || position.pending_fee_y != 0
        || position.data_len
            != meteora::position_data_len_for_width(meteora::POSITION_TARGET_WIDTH)?
        || position.lamports != rpc.get_minimum_balance_for_rent_exemption(position.data_len)?
    {
        bail!("Meteora position does not match the persistent zero-liquidity manifest");
    }
    for bin_array in &meteora_plan.bin_arrays {
        if !account_exists_with_owner(
            rpc,
            *bin_array,
            loyal_actions::autonomous_vaults::METEORA_DLMM_PROGRAM_ID,
        )? {
            bail!("approved Meteora BinArray {bin_array} is absent");
        }
    }
    require_all_steps_finalized(
        "meteora",
        &state
            .meteora
            .as_ref()
            .context("Meteora state is missing")?
            .live_steps,
        if meteora_plan.policy_generation == meteora::METEORA_EXPANDED_POLICY_GENERATION {
            16
        } else {
            10
        },
    )?;
    println!(
        "module=meteora verdict=PASS policies={} position={} bin_arrays={}",
        3 + meteora_plan.additional_policy_shards.len() * 3,
        meteora_plan.position,
        meteora_plan.bin_arrays.len()
    );

    let return_plan = returns::load_plan(rpc, state, deployment.pubkey(), delegated.pubkey())?;
    for kind in [TreasuryReturnKind::Loyal, TreasuryReturnKind::Usdc] {
        returns::decode_and_verify_policy(
            rpc,
            &return_plan,
            kind,
            delegated.pubkey(),
            deployment.pubkey(),
            expected_return_allowance(state, &return_plan, kind),
        )?;
        let step = return_live_step(state, return_step_name(kind))?;
        verify_return_transaction_token_deltas(rpc, &return_plan, kind, step)?;
    }
    require_all_steps_finalized(
        "treasury-returns",
        &state
            .returns
            .as_ref()
            .context("treasury-return state is missing")?
            .live_steps,
        2,
    )?;
    println!("module=treasury-returns verdict=PASS policies=2 transfers=2");

    let policy_addresses = all_policy_addresses(state)?;
    if policy_addresses.len() != expected_policy_seed as usize {
        bail!("the final policy manifest does not match the Settings policy seed");
    }
    verify_all_recorded_signatures(rpc, state)?;
    println!("module=recorded-signatures verdict=PASS");
    println!("final_settings={settings}");
    println!("final_vault={vault}");
    for policy in policy_addresses {
        println!("final_policy={policy}");
    }
    println!("overall_verdict=PASS");
    Ok(())
}

fn sync_routing_control_plane(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let smart = state
        .smart_account
        .as_ref()
        .context("Smart Account record is missing")?;
    if smart.status != SmartAccountStatus::Finalized {
        bail!("Smart Account is not finalized");
    }

    let (settings, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    if settings.to_string() != smart.settings || vault.to_string() != smart.vault {
        bail!("Kamino plan does not match the recorded autonomous vault identity");
    }
    for kind in [
        KaminoPolicyKind::Operations,
        KaminoPolicyKind::InitObligation,
    ] {
        verify_policy_account(
            rpc,
            &plan,
            kind,
            settings,
            deployment.pubkey(),
            delegated.pubkey(),
        )?;
    }

    let kamino = state.kamino.as_ref().context("Kamino record is missing")?;
    let operations = kamino
        .operations_policy
        .as_ref()
        .context("Kamino operations policy record is missing")?;
    let setup = kamino
        .init_obligation_policy
        .as_ref()
        .context("Kamino init-obligation policy record is missing")?;
    if operations.status != PolicyStatus::Finalized || setup.status != PolicyStatus::Finalized {
        bail!("Kamino routing policies must both be finalized before control-plane sync");
    }

    let markets = vec![KAMINO_MAIN_MARKET.to_string()];
    let stable_mints = vec![USDC_MINT.to_string()];
    let common = |record: &PolicyRecord, route_mode: String| -> Result<PolicyMatchInput> {
        Ok(PolicyMatchInput {
            signature: record
                .creation_signature
                .clone()
                .context("finalized Kamino policy is missing its creation signature")?,
            slot: record
                .finalized_slot
                .context("finalized Kamino policy is missing its finalized slot")?,
            settings: smart.settings.clone(),
            authority: deployment.pubkey().to_string(),
            policy_seed: record.seed,
            policy_account: record.policy.clone(),
            vault_index: smart.vault_index,
            vault_pubkey: smart.vault.clone(),
            delegated_signers: vec![delegated.pubkey().to_string()],
            threshold: 1,
            route_modes: vec![route_mode],
            stable_mints: stable_mints.clone(),
            kamino_markets: markets.clone(),
            kamino_liquidity_mints: stable_mints.clone(),
            universe_preset: Some("autonomous_fixed_main_v1".to_owned()),
            risk_profile: Some("fixed_main_market".to_owned()),
            swap_lanes: serde_json::Value::Array(Vec::new()),
        })
    };

    let database_url = env::var("NEON_DATABASE_URL").context("NEON_DATABASE_URL is not set")?;
    let operations_match = common(operations, FIXED_KAMINO_MAIN_ROUTE_MODE.to_owned())?;
    let setup_match = common(setup, format!("{FIXED_KAMINO_MAIN_ROUTE_MODE}_setup"))?;
    let runtime = tokio::runtime::Runtime::new().context("create routing sync runtime")?;
    let (stored, stored_setup) = runtime.block_on(async {
        let client = NeonSqlClient::connect(NeonSqlConfig::new(database_url)).await?;
        client
            .record_route_and_setup_policy_match(operations_match, setup_match)
            .await
    })?;

    println!(
        "routing_control_plane_sync=PASS vault_id={} route_policy_id={} setup_policy_id={} route_mode={} optimizer_eligible=false transactions_sent=false",
        stored.vault.id.as_i64(),
        stored.policy.id.as_i64(),
        stored_setup.id.as_i64(),
        FIXED_KAMINO_MAIN_ROUTE_MODE,
    );
    Ok(())
}

fn simulate_signer_handoff_readiness(
    rpc: &RpcClient,
    path: &std::path::Path,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    verify_all(rpc, path, state, deployment, delegated)?;

    let smart = state
        .smart_account
        .as_ref()
        .context("Smart Account record is missing")?;
    let settings = Pubkey::from_str(&smart.settings)?;
    let mother = loyal_actions::autonomous_vaults::MOTHER_TREASURY_VAULT;
    let mother_multisig = Pubkey::from_str("Gv27nnaXR8UanJmjPZ4MLS81eqee2DfzJSv7C8PkQTEC")?;
    let mother_vault_index = 0u8;
    if derive_squads_v4_vault(&mother_multisig, mother_vault_index).0 != mother {
        bail!("published Mother address is not the expected Squads v4 vault PDA");
    }
    let mother_multisig_account = rpc.get_account(&mother_multisig)?;
    if mother_multisig_account.owner != SQUADS_V4_PROGRAM_ID {
        bail!("Mother multisig is not owned by the Squads v4 program");
    }

    let settings_before = rpc.get_account(&settings)?;
    let decoded_before = squads::verify_created_settings(
        settings_before.owner,
        &settings_before.data,
        smart.account_index.parse::<u128>()?,
        deployment.pubkey(),
    )?;
    if decoded_before.policy_seed != Some(13) {
        bail!("signer handoff is forbidden before Settings policy seed 13");
    }

    let handoff = handoff_settings_signer_instruction(settings, deployment.pubkey(), mother)
        .map_err(anyhow::Error::msg)?;
    let decoded_handoff =
        decode_settings_signer_handoff_instruction(&handoff).map_err(anyhow::Error::msg)?;
    if decoded_handoff.settings != settings
        || decoded_handoff.current_signer != deployment.pubkey()
        || decoded_handoff.new_signer != mother
        || decoded_handoff.new_signer_permissions_mask != 7
    {
        bail!("decoded signer handoff does not match the exact reviewed manifest");
    }

    let blockhash = rpc.get_latest_blockhash()?;
    let transaction = Transaction::new_signed_with_payer(
        &[handoff],
        Some(&deployment.pubkey()),
        &[deployment],
        blockhash,
    );
    let packet_bytes = bincode::serialized_size(&transaction)?;
    if packet_bytes > SOLANA_PACKET_DATA_SIZE {
        bail!("signer handoff exceeds Solana packet size: {packet_bytes}");
    }
    let units = simulate_signed_transaction(rpc, &transaction, "signer handoff readiness")?;
    let settings_after = rpc.get_account(&settings)?;
    if settings_after != settings_before {
        bail!("signed-unsent handoff simulation changed live Settings state");
    }

    println!("module=signer-handoff-readiness verdict=PASS");
    println!("mother_squads_v4_program={SQUADS_V4_PROGRAM_ID}");
    println!("mother_multisig={mother_multisig}");
    println!("mother_vault_index={mother_vault_index}");
    println!("mother_vault={mother}");
    println!("handoff_add_signer={}", decoded_handoff.new_signer);
    println!("handoff_add_permissions_mask=7");
    println!("handoff_remove_signer={}", decoded_handoff.current_signer);
    println!("handoff_packet_bytes={packet_bytes}");
    println!("handoff_units_consumed={units}");
    println!("handoff_ready=PASS transaction_sent=false");
    Ok(())
}

fn require_all_steps_finalized(
    module: &str,
    steps: &[LiveStepRecord],
    expected_count: usize,
) -> Result<()> {
    if steps.len() < expected_count
        || steps.iter().any(|step| {
            step.status != PolicyStatus::Finalized
                || step.finalized_signature.is_none()
                || step.finalized_slot.is_none()
        })
    {
        bail!("{module} does not have the complete finalized live-step manifest");
    }
    for step in steps {
        println!(
            "evidence module={} step={} signature={} slot={} before={:?} after={:?}",
            module,
            step.name,
            step.finalized_signature.as_deref().unwrap_or("missing"),
            step.finalized_slot.unwrap_or_default(),
            step.before,
            step.after
        );
    }
    Ok(())
}

fn all_policy_addresses(state: &VaultState) -> Result<BTreeSet<Pubkey>> {
    let kamino = state.kamino.as_ref().context("Kamino state is missing")?;
    let meteora = state.meteora.as_ref().context("Meteora state is missing")?;
    let returns = state
        .returns
        .as_ref()
        .context("treasury-return state is missing")?;
    let records = [
        kamino.operations_policy.as_ref(),
        kamino.init_obligation_policy.as_ref(),
        meteora.add_liquidity_policy.as_ref(),
        meteora.remove_liquidity_policy.as_ref(),
        meteora.claim_fee_policy.as_ref(),
        returns.loyal_policy.as_ref(),
        returns.usdc_policy.as_ref(),
    ];
    let mut addresses = records
        .into_iter()
        .map(|record| {
            let record = record.context("policy record is missing")?;
            if record.status != PolicyStatus::Finalized {
                bail!("policy record is not finalized");
            }
            Pubkey::from_str(&record.policy).context("parse policy address")
        })
        .collect::<Result<BTreeSet<_>>>()?;
    for shard in &meteora.additional_policy_shards {
        for record in [
            shard.add_liquidity_policy.as_ref(),
            shard.remove_liquidity_policy.as_ref(),
            shard.claim_fee_policy.as_ref(),
        ] {
            let record = record.context("Meteora shard policy record is missing")?;
            if record.status != PolicyStatus::Finalized {
                bail!("Meteora shard policy record is not finalized");
            }
            if !addresses.insert(Pubkey::from_str(&record.policy)?) {
                bail!("policy manifest contains a duplicate PDA");
            }
        }
    }
    Ok(addresses)
}

fn verify_all_recorded_signatures(rpc: &RpcClient, state: &VaultState) -> Result<()> {
    let mut evidence = Vec::<(String, Signature, u64)>::new();
    let smart = state
        .smart_account
        .as_ref()
        .context("Smart Account state is missing")?;
    evidence.push((
        "smart-account-create".to_owned(),
        Signature::from_str(
            smart
                .creation_signature
                .as_deref()
                .context("Smart Account creation signature is missing")?,
        )?,
        smart
            .finalized_slot
            .context("Smart Account creation slot is missing")?,
    ));

    let kamino = state.kamino.as_ref().context("Kamino state is missing")?;
    let meteora = state.meteora.as_ref().context("Meteora state is missing")?;
    let returns = state
        .returns
        .as_ref()
        .context("treasury-return state is missing")?;
    for (label, record) in [
        (
            "kamino-operations-policy",
            kamino.operations_policy.as_ref(),
        ),
        ("kamino-init-policy", kamino.init_obligation_policy.as_ref()),
        ("meteora-add-policy", meteora.add_liquidity_policy.as_ref()),
        (
            "meteora-remove-policy",
            meteora.remove_liquidity_policy.as_ref(),
        ),
        ("meteora-claim-policy", meteora.claim_fee_policy.as_ref()),
        ("return-loyal-policy", returns.loyal_policy.as_ref()),
        ("return-usdc-policy", returns.usdc_policy.as_ref()),
    ] {
        let record = record.context("policy signature record is missing")?;
        evidence.push((
            label.to_owned(),
            Signature::from_str(
                record
                    .creation_signature
                    .as_deref()
                    .context("policy creation signature is missing")?,
            )?,
            record
                .finalized_slot
                .context("policy creation slot is missing")?,
        ));
    }
    for shard in &meteora.additional_policy_shards {
        for (kind, record) in [
            ("add", shard.add_liquidity_policy.as_ref()),
            ("remove", shard.remove_liquidity_policy.as_ref()),
            ("claim", shard.claim_fee_policy.as_ref()),
        ] {
            let record = record.context("Meteora shard policy signature record is missing")?;
            evidence.push((
                format!("meteora-shard-{}-{kind}-policy", shard.shard_index),
                Signature::from_str(
                    record
                        .creation_signature
                        .as_deref()
                        .context("Meteora shard policy creation signature is missing")?,
                )?,
                record
                    .finalized_slot
                    .context("Meteora shard policy creation slot is missing")?,
            ));
        }
    }
    for (module, steps) in [
        ("kamino", kamino.live_steps.as_slice()),
        ("meteora", meteora.live_steps.as_slice()),
        ("treasury-return", returns.live_steps.as_slice()),
    ] {
        for step in steps {
            evidence.push((
                format!("{module}:{}", step.name),
                Signature::from_str(
                    step.finalized_signature
                        .as_deref()
                        .context("live-step signature is missing")?,
                )?,
                step.finalized_slot.context("live-step slot is missing")?,
            ));
        }
    }

    let signatures = evidence
        .iter()
        .map(|(_, signature, _)| *signature)
        .collect::<Vec<_>>();
    let statuses = rpc.get_signature_statuses(&signatures)?;
    for ((label, signature, expected_slot), status) in
        evidence.iter().zip(statuses.value.into_iter())
    {
        let status = status.with_context(|| format!("signature {label} is not visible"))?;
        if status.err.is_some()
            || !status.satisfies_commitment(CommitmentConfig::finalized())
            || status.slot != *expected_slot
        {
            bail!("recorded signature {label} does not match finalized RPC evidence");
        }
        println!("signature label={label} value={signature} slot={expected_slot}");
    }
    Ok(())
}

fn verify_return_transaction_token_deltas(
    rpc: &RpcClient,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    step: &LiveStepRecord,
) -> Result<()> {
    let signature = Signature::from_str(
        step.finalized_signature
            .as_deref()
            .context("return signature is missing")?,
    )?;
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    if Some(transaction.slot) != step.finalized_slot {
        bail!("return transaction slot does not match the durable record");
    }
    let encoded = serde_json::to_value(&transaction)?;
    let meta = encoded
        .pointer("/meta")
        .context("return transaction metadata is absent")?;
    if !meta
        .get("err")
        .map(serde_json::Value::is_null)
        .unwrap_or(false)
    {
        bail!("return transaction metadata reports an error");
    }
    let pre = meta
        .get("preTokenBalances")
        .and_then(serde_json::Value::as_array)
        .context("return transaction pre-token balances are absent")?;
    let post = meta
        .get("postTokenBalances")
        .and_then(serde_json::Value::as_array)
        .context("return transaction post-token balances are absent")?;
    let policy = plan.policy(kind);
    let vault_pre = transaction_token_balance(pre, plan.vault, policy.mint)?;
    let vault_post = transaction_token_balance(post, plan.vault, policy.mint)?;
    let mother_pre = transaction_token_balance(pre, policy.destination_owner, policy.mint)?;
    let mother_post = transaction_token_balance(post, policy.destination_owner, policy.mint)?;
    let amount = plan.amount(kind);
    if vault_pre.checked_sub(amount) != Some(vault_post)
        || mother_pre.checked_add(amount) != Some(mother_post)
    {
        bail!("return transaction metadata does not contain the exact token deltas");
    }
    println!(
        "return_transaction kind={} signature={} slot={} vault={}->{} mother={}->{} amount={}",
        kind.label(),
        signature,
        transaction.slot,
        vault_pre,
        vault_post,
        mother_pre,
        mother_post,
        amount
    );
    Ok(())
}

fn transaction_token_balance(
    balances: &[serde_json::Value],
    owner: Pubkey,
    mint: Pubkey,
) -> Result<u64> {
    let owner = owner.to_string();
    let mint = mint.to_string();
    balances
        .iter()
        .find(|balance| {
            balance.get("owner").and_then(serde_json::Value::as_str) == Some(owner.as_str())
                && balance.get("mint").and_then(serde_json::Value::as_str) == Some(mint.as_str())
        })
        .and_then(|balance| balance.pointer("/uiTokenAmount/amount"))
        .and_then(serde_json::Value::as_str)
        .context("expected owner/mint token balance is absent from transaction metadata")?
        .parse::<u64>()
        .context("parse raw transaction token balance")
}

fn simulate_return_policy(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: TreasuryReturnKind,
) -> Result<()> {
    let plan = returns::load_plan(rpc, state, deployment.pubkey(), delegated.pubkey())?;
    enforce_return_policy_prerequisite(state, kind)?;
    let policy = plan.policy(kind);
    if rpc.get_account(&policy.policy).is_ok() {
        bail!(
            "{} return policy already exists; inspect it instead",
            kind.label()
        );
    }
    verify_next_policy_seed(rpc, plan.settings, policy.policy_seed)?;
    let (transaction, _, _) =
        build_policy_transaction(rpc, &policy.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    println!(
        "module=return-{}-policy-simulation verdict=PASS",
        kind.label()
    );
    println!("policy={}", policy.policy);
    println!("policy_seed={}", policy.policy_seed);
    println!("packet_bytes={}", bincode::serialized_size(&transaction)?);
    println!("units_consumed={units}");
    println!("transaction_sent=false");
    Ok(())
}

fn create_or_resume_return_policy(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: TreasuryReturnKind,
) -> Result<()> {
    let plan = returns::load_plan(rpc, state, deployment.pubkey(), delegated.pubkey())?;
    ensure_return_record(path, state, &plan)?;
    enforce_return_policy_prerequisite(state, kind)?;
    let policy_plan = plan.policy(kind);
    let record = state
        .returns
        .as_mut()
        .context("treasury-return record is missing")?;
    if returns::policy_record(record, kind).is_none() {
        *returns::policy_record_mut(record, kind) = Some(PolicyRecord {
            status: PolicyStatus::Planned,
            seed: policy_plan.policy_seed,
            policy: policy_plan.policy.to_string(),
            pending_signature: None,
            last_valid_block_height: None,
            creation_signature: None,
            finalized_slot: None,
        });
        state::save(path, state)?;
    }

    let record = returns::policy_record(
        state
            .returns
            .as_ref()
            .context("treasury-return record is missing")?,
        kind,
    )
    .context("return policy record is missing")?;
    if record.seed != policy_plan.policy_seed || record.policy != policy_plan.policy.to_string() {
        bail!(
            "recorded {} return policy differs from the fresh plan",
            kind.label()
        );
    }
    if record.status == PolicyStatus::Finalized {
        returns::decode_and_verify_policy(
            rpc,
            &plan,
            kind,
            delegated.pubkey(),
            deployment.pubkey(),
            expected_return_allowance(state, &plan, kind),
        )?;
        println!("{} return policy is already finalized", kind.label());
        return Ok(());
    }
    if let Some(signature) = record.pending_signature.as_deref() {
        let signature = Signature::from_str(signature)?;
        if let Some(status) = rpc
            .get_signature_statuses(&[signature])?
            .value
            .into_iter()
            .next()
            .flatten()
        {
            if let Some(error) = status.err {
                bail!(
                    "recorded {} return policy creation failed: {error:?}",
                    kind.label()
                );
            }
            if status.satisfies_commitment(CommitmentConfig::finalized()) {
                return finalize_return_policy_record(
                    rpc,
                    path,
                    state,
                    &plan,
                    kind,
                    deployment.pubkey(),
                    delegated.pubkey(),
                    signature,
                );
            }
            bail!(
                "recorded {} return policy is not finalized yet",
                kind.label()
            );
        }
        let last_valid = record
            .last_valid_block_height
            .context("pending return policy is missing last valid block height")?;
        if rpc.get_block_height()? <= last_valid {
            bail!("recorded return policy signature is still live but not visible");
        }
    }
    if rpc.get_account(&policy_plan.policy).is_ok() {
        bail!("planned return policy exists without recoverable finalized evidence");
    }

    verify_next_policy_seed(rpc, plan.settings, policy_plan.policy_seed)?;
    let (transaction, blockhash, last_valid_block_height) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    println!(
        "return_{}_policy_simulation=PASS units_consumed={units}",
        kind.label()
    );
    println!("packet_bytes={}", bincode::serialized_size(&transaction)?);
    let pending = transaction.signatures[0];
    {
        let record = returns::policy_record_mut(
            state
                .returns
                .as_mut()
                .context("treasury-return record is missing")?,
            kind,
        )
        .as_mut()
        .context("return policy record is missing")?;
        record.pending_signature = Some(pending.to_string());
        record.last_valid_block_height = Some(last_valid_block_height);
    }
    state::save(path, state)?;
    let sent = rpc.send_transaction_with_config(
        &transaction,
        RpcSendTransactionConfig {
            skip_preflight: false,
            preflight_commitment: Some(CommitmentLevel::Finalized),
            ..RpcSendTransactionConfig::default()
        },
    )?;
    if sent != pending {
        bail!("RPC returned a different return-policy signature");
    }
    rpc.confirm_transaction_with_spinner(&sent, &blockhash, CommitmentConfig::finalized())?;
    finalize_return_policy_record(
        rpc,
        path,
        state,
        &plan,
        kind,
        deployment.pubkey(),
        delegated.pubkey(),
        sent,
    )
}

#[allow(clippy::too_many_arguments)]
fn finalize_return_policy_record(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    deployment: Pubkey,
    delegated: Pubkey,
    signature: Signature,
) -> Result<()> {
    let decoded =
        returns::decode_and_verify_policy(rpc, plan, kind, delegated, deployment, Some(u64::MAX))?;
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let record = returns::policy_record_mut(
        state
            .returns
            .as_mut()
            .context("treasury-return record is missing")?,
        kind,
    )
    .as_mut()
    .context("return policy record is missing")?;
    record.status = PolicyStatus::Finalized;
    record.creation_signature = Some(signature.to_string());
    record.finalized_slot = Some(transaction.slot);
    state::save(path, state)?;
    println!(
        "create_return_{}_policy=PASS policy={} signature={} slot={} remaining={}",
        kind.label(),
        plan.policy(kind).policy,
        signature,
        transaction.slot,
        decoded.remaining_in_period
    );
    Ok(())
}

fn enforce_return_policy_prerequisite(state: &VaultState, kind: TreasuryReturnKind) -> Result<()> {
    if kind == TreasuryReturnKind::Usdc {
        let loyal = state
            .returns
            .as_ref()
            .and_then(|record| record.loyal_policy.as_ref());
        if loyal.map(|policy| policy.status) != Some(PolicyStatus::Finalized) {
            bail!("LOYAL return policy must finalize before the USDC return policy");
        }
    }
    Ok(())
}

fn ensure_return_record(
    path: &std::path::PathBuf,
    state: &mut VaultState,
    plan: &returns::TreasuryReturnPlan,
) -> Result<()> {
    match &state.returns {
        Some(record) => returns::validate_record(record, plan),
        None => {
            state.returns = Some(returns::record_from_plan(plan));
            state::save(path, state)
        }
    }
}

fn simulate_return_execution(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: TreasuryReturnKind,
) -> Result<()> {
    let plan = returns::load_plan(rpc, state, deployment.pubkey(), delegated.pubkey())?;
    require_finalized_return_policies(state)?;
    enforce_return_execution_prerequisite(state, kind)?;
    let policy = plan.policy(kind);
    let amount = plan.amount(kind);
    returns::decode_and_verify_policy(
        rpc,
        &plan,
        kind,
        delegated.pubkey(),
        deployment.pubkey(),
        expected_return_allowance(state, &plan, kind),
    )?;
    let source_amount =
        token_account_amount(rpc, policy.source_token_account, plan.vault, policy.mint)?
            .context("vault return ATA disappeared")?;
    if source_amount < amount {
        bail!(
            "vault has insufficient {} dust for return proof",
            kind.label()
        );
    }

    let instruction = return_to_mother_instruction(
        plan.settings,
        delegated.pubkey(),
        plan.vault_index,
        policy,
        amount,
    );
    let (transaction, _, _) =
        build_return_transaction(rpc, instruction.clone(), deployment, delegated)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    simulate_rejected_return_mutations(rpc, &plan, kind, instruction, deployment, delegated)?;
    println!(
        "module=return-{}-execution-simulation verdict=PASS",
        kind.label()
    );
    println!("amount_raw={amount}");
    println!("packet_bytes={}", bincode::serialized_size(&transaction)?);
    println!("units_consumed={units}");
    println!("wrong_destination_simulation=REJECTED");
    println!("wrong_mint_simulation=REJECTED");
    println!("wrong_signer_simulation=REJECTED");
    println!("transaction_sent=false");
    Ok(())
}

fn execute_return_to_mother(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: TreasuryReturnKind,
) -> Result<()> {
    let plan = returns::load_plan(rpc, state, deployment.pubkey(), delegated.pubkey())?;
    ensure_return_record(path, state, &plan)?;
    require_finalized_return_policies(state)?;
    enforce_return_execution_prerequisite(state, kind)?;
    let step_name = return_step_name(kind);
    let fresh_before = return_observations(rpc, &plan, kind)?;
    ensure_return_live_step(path, state, step_name, fresh_before)?;
    let before = return_live_step(state, step_name)?.before.clone();
    if let Some(signature) = recover_finalized_return_step(
        rpc,
        state,
        &plan,
        kind,
        step_name,
        deployment.pubkey(),
        delegated.pubkey(),
    )? {
        let after = verify_return_deltas(rpc, &plan, kind, &before)?;
        return finalize_return_step(
            rpc,
            path,
            state,
            &plan,
            kind,
            signature,
            after,
            deployment.pubkey(),
            delegated.pubkey(),
        );
    }
    if return_live_step(state, step_name)?.status == PolicyStatus::Finalized {
        verify_return_deltas(
            rpc,
            &plan,
            kind,
            &return_live_step(state, step_name)?.before,
        )?;
        returns::decode_and_verify_policy(
            rpc,
            &plan,
            kind,
            delegated.pubkey(),
            deployment.pubkey(),
            expected_return_allowance(state, &plan, kind),
        )?;
        println!("{step_name}=PASS already_finalized=true");
        return Ok(());
    }

    let policy = plan.policy(kind);
    let amount = plan.amount(kind);
    if before.get("vault_raw").copied().unwrap_or(0) < amount {
        bail!(
            "vault has insufficient {} dust for return proof",
            kind.label()
        );
    }
    let instruction = return_to_mother_instruction(
        plan.settings,
        delegated.pubkey(),
        plan.vault_index,
        policy,
        amount,
    );
    let (transaction, blockhash, last_valid_block_height) =
        build_return_transaction(rpc, instruction, deployment, delegated)?;
    let units = simulate_signed_transaction(rpc, &transaction, step_name)?;
    println!("{step_name}_simulation=PASS units_consumed={units}");
    let pending = transaction.signatures[0];
    {
        let step = return_live_step_mut(state, step_name)?;
        step.pending_signature = Some(pending.to_string());
        step.last_valid_block_height = Some(last_valid_block_height);
    }
    state::save(path, state)?;
    let sent = rpc.send_transaction_with_config(
        &transaction,
        RpcSendTransactionConfig {
            skip_preflight: false,
            preflight_commitment: Some(CommitmentLevel::Finalized),
            ..RpcSendTransactionConfig::default()
        },
    )?;
    if sent != pending {
        bail!("RPC returned a different treasury-return signature");
    }
    rpc.confirm_transaction_with_spinner(&sent, &blockhash, CommitmentConfig::finalized())?;
    let after = verify_return_deltas(rpc, &plan, kind, &before)?;
    finalize_return_step(
        rpc,
        path,
        state,
        &plan,
        kind,
        sent,
        after,
        deployment.pubkey(),
        delegated.pubkey(),
    )
}

fn require_finalized_return_policies(state: &VaultState) -> Result<()> {
    let returns = state
        .returns
        .as_ref()
        .context("treasury-return state is missing")?;
    for (label, policy) in [
        ("LOYAL", returns.loyal_policy.as_ref()),
        ("USDC", returns.usdc_policy.as_ref()),
    ] {
        if policy.map(|record| record.status) != Some(PolicyStatus::Finalized) {
            bail!("{label} return policy must be finalized before delegated execution");
        }
    }
    Ok(())
}

fn enforce_return_execution_prerequisite(
    state: &VaultState,
    kind: TreasuryReturnKind,
) -> Result<()> {
    if kind == TreasuryReturnKind::Usdc {
        let loyal = state.returns.as_ref().and_then(|record| {
            record
                .live_steps
                .iter()
                .find(|step| step.name == return_step_name(TreasuryReturnKind::Loyal))
        });
        if loyal.map(|step| step.status) != Some(PolicyStatus::Finalized) {
            bail!("LOYAL return proof must finalize before the USDC return proof");
        }
    }
    Ok(())
}

fn return_step_name(kind: TreasuryReturnKind) -> &'static str {
    match kind {
        TreasuryReturnKind::Loyal => "return-loyal-to-mother",
        TreasuryReturnKind::Usdc => "return-usdc-to-mother",
    }
}

fn return_observations(
    rpc: &RpcClient,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
) -> Result<BTreeMap<String, u64>> {
    let policy = plan.policy(kind);
    let mut observations = BTreeMap::new();
    observations.insert(
        "vault_raw".to_owned(),
        token_account_amount(rpc, policy.source_token_account, plan.vault, policy.mint)?
            .context("vault return ATA disappeared")?,
    );
    observations.insert(
        "mother_raw".to_owned(),
        token_account_amount(
            rpc,
            policy.destination_token_account,
            policy.destination_owner,
            policy.mint,
        )?
        .context("Mother return ATA disappeared")?,
    );
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&plan.vault)?);
    Ok(observations)
}

fn verify_return_deltas(
    rpc: &RpcClient,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = return_observations(rpc, plan, kind)?;
    let amount = plan.amount(kind);
    if before
        .get("vault_raw")
        .and_then(|value| value.checked_sub(amount))
        != after.get("vault_raw").copied()
        || before
            .get("mother_raw")
            .and_then(|value| value.checked_add(amount))
            != after.get("mother_raw").copied()
        || before.get("vault_lamports") != after.get("vault_lamports")
    {
        bail!(
            "{} treasury-return deltas do not match the exact manifest",
            kind.label()
        );
    }
    Ok(after)
}

fn ensure_return_live_step(
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    before: BTreeMap<String, u64>,
) -> Result<()> {
    let steps = &mut state
        .returns
        .as_mut()
        .context("treasury-return state is missing")?
        .live_steps;
    if steps.iter().all(|step| step.name != name) {
        steps.push(LiveStepRecord {
            name: name.to_owned(),
            status: PolicyStatus::Planned,
            pending_signature: None,
            last_valid_block_height: None,
            finalized_signature: None,
            finalized_slot: None,
            before,
            after: BTreeMap::new(),
        });
        state::save(path, state)?;
    }
    Ok(())
}

fn return_live_step<'a>(state: &'a VaultState, name: &str) -> Result<&'a LiveStepRecord> {
    state
        .returns
        .as_ref()
        .and_then(|record| record.live_steps.iter().find(|step| step.name == name))
        .with_context(|| format!("treasury-return live step {name} is missing"))
}

fn return_live_step_mut<'a>(
    state: &'a mut VaultState,
    name: &str,
) -> Result<&'a mut LiveStepRecord> {
    state
        .returns
        .as_mut()
        .and_then(|record| record.live_steps.iter_mut().find(|step| step.name == name))
        .with_context(|| format!("treasury-return live step {name} is missing"))
}

fn recover_finalized_return_step(
    rpc: &RpcClient,
    state: &VaultState,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    name: &str,
    deployment: Pubkey,
    delegated: Pubkey,
) -> Result<Option<Signature>> {
    let step = return_live_step(state, name)?;
    if step.status == PolicyStatus::Finalized {
        return Ok(None);
    }
    let Some(signature) = step.pending_signature.as_deref() else {
        return Ok(None);
    };
    let signature = Signature::from_str(signature)?;
    let statuses = rpc.get_signature_statuses(&[signature])?;
    if let Some(status) = statuses.value.into_iter().next().flatten() {
        if let Some(error) = status.err {
            bail!("recorded {name} transaction failed on chain: {error:?}");
        }
        if status.satisfies_commitment(CommitmentConfig::finalized()) {
            return Ok(Some(signature));
        }
        bail!("recorded {name} transaction is not finalized yet");
    }
    let current = rpc.get_block_height()?;
    let last_valid = step
        .last_valid_block_height
        .context("pending return step is missing last valid block height")?;
    if current <= last_valid {
        bail!("recorded {name} signature is still live but not visible");
    }
    if let Ok(transaction) = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    ) {
        if transaction
            .transaction
            .meta
            .as_ref()
            .and_then(|meta| meta.err.as_ref())
            .is_some()
        {
            bail!("recorded {name} transaction finalized with an error");
        }
        return Ok(Some(signature));
    }

    let current_balances = return_observations(rpc, plan, kind)?;
    let decoded = returns::decode_and_verify_policy(rpc, plan, kind, delegated, deployment, None)?;
    if current_balances == step.before && decoded.remaining_in_period == u64::MAX {
        return Ok(None);
    }
    bail!(
        "recorded {name} signature expired from status history with ambiguous balance or allowance changes; refusing to resend"
    )
}

#[allow(clippy::too_many_arguments)]
fn finalize_return_step(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    signature: Signature,
    after: BTreeMap<String, u64>,
    deployment: Pubkey,
    delegated: Pubkey,
) -> Result<()> {
    let decoded = returns::decode_and_verify_policy(
        rpc,
        plan,
        kind,
        delegated,
        deployment,
        Some(u64::MAX - plan.amount(kind)),
    )?;
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let step = return_live_step_mut(state, return_step_name(kind))?;
    step.status = PolicyStatus::Finalized;
    step.finalized_signature = Some(signature.to_string());
    step.finalized_slot = Some(transaction.slot);
    step.after = after.clone();
    state::save(path, state)?;
    println!(
        "{}=PASS signature={} slot={} amount_raw={} vault_before={} vault_after={} mother_before={} mother_after={} remaining_allowance={}",
        return_step_name(kind),
        signature,
        transaction.slot,
        plan.amount(kind),
        return_live_step(state, return_step_name(kind))?.before["vault_raw"],
        after["vault_raw"],
        return_live_step(state, return_step_name(kind))?.before["mother_raw"],
        after["mother_raw"],
        decoded.remaining_in_period,
    );
    Ok(())
}

fn expected_return_allowance(
    state: &VaultState,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
) -> Option<u64> {
    let executed = state.returns.as_ref().and_then(|record| {
        record
            .live_steps
            .iter()
            .find(|step| step.name == return_step_name(kind))
    });
    match executed.map(|step| step.status) {
        Some(PolicyStatus::Finalized) => Some(u64::MAX - plan.amount(kind)),
        Some(PolicyStatus::Planned) => None,
        None => Some(u64::MAX),
    }
}

fn build_return_transaction(
    rpc: &RpcClient,
    instruction: Instruction,
    deployment: &solana_sdk::signature::Keypair,
    policy_signer: &solana_sdk::signature::Keypair,
) -> Result<(Transaction, solana_sdk::hash::Hash, u64)> {
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let transaction = if deployment.pubkey() == policy_signer.pubkey() {
        Transaction::new_signed_with_payer(
            &[instruction],
            Some(&deployment.pubkey()),
            &[deployment],
            blockhash,
        )
    } else {
        Transaction::new_signed_with_payer(
            &[instruction],
            Some(&deployment.pubkey()),
            &[deployment, policy_signer],
            blockhash,
        )
    };
    let packet_size = bincode::serialized_size(&transaction)?;
    if packet_size > SOLANA_PACKET_DATA_SIZE {
        bail!("return transaction exceeds Solana packet size: {packet_size}");
    }
    Ok((transaction, blockhash, last_valid_block_height))
}

fn simulate_rejected_return_mutations(
    rpc: &RpcClient,
    plan: &returns::TreasuryReturnPlan,
    kind: TreasuryReturnKind,
    instruction: Instruction,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let mut wrong_destination = instruction.clone();
    let destination_offset = 20;
    let end = destination_offset + 32;
    if wrong_destination.data.len() <= end {
        bail!("SpendingLimit payload is shorter than the reviewed wire layout");
    }
    wrong_destination.data[destination_offset..end].copy_from_slice(Pubkey::new_unique().as_ref());
    require_failed_return_simulation(
        rpc,
        wrong_destination,
        deployment,
        delegated,
        "wrong destination",
    )?;

    let mut wrong_mint = instruction.clone();
    wrong_mint.accounts[6].pubkey = match kind {
        TreasuryReturnKind::Loyal => loyal_actions::USDC_MINT,
        TreasuryReturnKind::Usdc => loyal_actions::autonomous_vaults::METEORA_LOYAL_MINT,
    };
    require_failed_return_simulation(rpc, wrong_mint, deployment, delegated, "wrong mint")?;

    let wrong_signer = solana_sdk::signature::Keypair::new();
    let mut wrong_signer_instruction = instruction;
    wrong_signer_instruction.accounts[2].pubkey = wrong_signer.pubkey();
    require_failed_return_simulation(
        rpc,
        wrong_signer_instruction,
        deployment,
        &wrong_signer,
        "wrong signer",
    )?;
    let _ = plan;
    Ok(())
}

fn require_failed_return_simulation(
    rpc: &RpcClient,
    instruction: Instruction,
    deployment: &solana_sdk::signature::Keypair,
    policy_signer: &solana_sdk::signature::Keypair,
    label: &str,
) -> Result<()> {
    let (transaction, _, _) =
        build_return_transaction(rpc, instruction, deployment, policy_signer)?;
    let simulation = rpc.simulate_transaction_with_config(
        &transaction,
        RpcSimulateTransactionConfig {
            sig_verify: true,
            replace_recent_blockhash: false,
            commitment: Some(CommitmentConfig::finalized()),
            ..RpcSimulateTransactionConfig::default()
        },
    )?;
    if simulation.value.err.is_none() {
        bail!("{label} return mutation unexpectedly passed simulation");
    }
    Ok(())
}

fn inspect_kamino(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (settings, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    let settings_account = rpc.get_account(&settings)?;
    let decoded_settings = squads::decode_settings(&settings_account.data)?;
    println!("module=kamino-readiness verdict=PASS");
    println!("settings={settings}");
    println!("vault={vault}");
    println!("vault_usdc_token_account={}", plan.vault_usdc);
    println!("reserve_source_slot={}", plan.source_slot);
    println!(
        "settings_policy_seed={}",
        decoded_settings
            .policy_seed
            .map(|seed| seed.to_string())
            .as_deref()
            .unwrap_or("none")
    );
    for reserve in &plan.reserves {
        println!(
            "kamino_pair market={} reserve={} obligation={} farm={} farm_user={}",
            reserve.decoded.market,
            reserve.decoded.reserve,
            reserve.obligation,
            reserve
                .decoded
                .collateral_farm
                .map(|key| key.to_string())
                .as_deref()
                .unwrap_or("none"),
            reserve
                .obligation_farm_user_state
                .map(|key| key.to_string())
                .as_deref()
                .unwrap_or("none")
        );
    }
    for kind in [
        KaminoPolicyKind::Operations,
        KaminoPolicyKind::InitObligation,
    ] {
        let (policy_plan, _) = plan_policy(&plan, kind);
        let record = state
            .kamino
            .as_ref()
            .and_then(|kamino| policy_record(kamino, kind));
        match record {
            Some(record) if record.status == PolicyStatus::Finalized => {
                let decoded = verify_policy_account(
                    rpc,
                    &plan,
                    kind,
                    settings,
                    deployment.pubkey(),
                    delegated.pubkey(),
                )?;
                println!(
                    "policy={} status=PASS address={} seed={} transaction_index={} start={} rent_collector={}",
                    kind.label(),
                    record.policy,
                    record.seed,
                    decoded.transaction_index,
                    decoded.start,
                    decoded.rent_collector
                );
            }
            Some(record) => println!(
                "policy={} status={:?} address={} seed={}",
                kind.label(),
                record.status,
                record.policy,
                record.seed
            ),
            None => println!(
                "policy={} status=PENDING address={} seed={}",
                kind.label(),
                policy_plan.policy,
                policy_plan.policy_seed
            ),
        }
    }
    Ok(())
}

fn simulate_kamino_policy(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: KaminoPolicyKind,
) -> Result<()> {
    let (settings, _, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    enforce_policy_prerequisites(state, kind)?;
    let (policy_plan, _) = plan_policy(&plan, kind);
    if rpc.get_account(&policy_plan.policy).is_ok() {
        bail!(
            "{} policy account already exists; inspect it instead",
            kind.label()
        );
    }
    verify_next_policy_seed(rpc, settings, kind.seed())?;
    let (transaction, _, _) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    println!("module={} policy-simulation verdict=PASS", kind.label());
    println!("policy={}", policy_plan.policy);
    println!("policy_seed={}", policy_plan.policy_seed);
    println!("units_consumed={units}");
    println!("transaction_sent=false");
    Ok(())
}

fn create_or_resume_kamino_policy(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    kind: KaminoPolicyKind,
) -> Result<()> {
    let (settings, _, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    if state.kamino.is_none() {
        state.kamino = Some(kamino::record_from_plan(&plan));
        state::save(path, state)?;
    }
    enforce_policy_prerequisites(state, kind)?;
    let (policy_plan, _) = plan_policy(&plan, kind);
    if policy_record(
        state.kamino.as_ref().context("Kamino record is missing")?,
        kind,
    )
    .is_none()
    {
        *policy_record_mut(
            state.kamino.as_mut().context("Kamino record is missing")?,
            kind,
        ) = Some(PolicyRecord {
            status: PolicyStatus::Planned,
            seed: kind.seed(),
            policy: policy_plan.policy.to_string(),
            pending_signature: None,
            last_valid_block_height: None,
            creation_signature: None,
            finalized_slot: None,
        });
        state::save(path, state)?;
    }

    let record = policy_record(
        state.kamino.as_ref().context("Kamino record is missing")?,
        kind,
    )
    .context("policy record is missing")?;
    if record.seed != kind.seed() || record.policy != policy_plan.policy.to_string() {
        bail!(
            "recorded {} policy identity does not match the fresh plan",
            kind.label()
        );
    }
    if record.status == PolicyStatus::Finalized {
        verify_policy_account(
            rpc,
            &plan,
            kind,
            settings,
            deployment.pubkey(),
            delegated.pubkey(),
        )?;
        println!(
            "{} policy is already finalized; refusing to create another",
            kind.label()
        );
        return Ok(());
    }

    if let Some(signature) = record.pending_signature.as_deref() {
        let signature = Signature::from_str(signature).context("parse pending policy signature")?;
        let statuses = rpc.get_signature_statuses(&[signature])?;
        if let Some(status) = statuses.value.into_iter().next().flatten() {
            if let Some(error) = status.err {
                bail!(
                    "recorded {} policy creation failed on chain: {error:?}",
                    kind.label()
                );
            }
            if status.satisfies_commitment(CommitmentConfig::finalized()) {
                return finalize_policy_record(
                    rpc,
                    path,
                    state,
                    &plan,
                    kind,
                    settings,
                    deployment.pubkey(),
                    delegated.pubkey(),
                    signature,
                );
            }
            bail!(
                "recorded {} policy creation is not finalized yet",
                kind.label()
            );
        }
        let current_height = rpc.get_block_height()?;
        let last_valid = record
            .last_valid_block_height
            .context("pending policy is missing its last valid block height")?;
        if current_height <= last_valid {
            bail!("recorded policy signature is still live but not visible; retry later");
        }
    }

    if rpc.get_account(&policy_plan.policy).is_ok() {
        bail!("planned policy exists without a recoverable finalized signature");
    }
    verify_next_policy_seed(rpc, settings, kind.seed())?;
    let (transaction, blockhash, last_valid_block_height) =
        build_policy_transaction(rpc, &policy_plan.create_instruction, deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, kind.label())?;
    println!("{}_simulation=PASS units_consumed={units}", kind.label());

    let pending_signature = transaction.signatures[0];
    let record = policy_record_mut(
        state.kamino.as_mut().context("Kamino record is missing")?,
        kind,
    )
    .as_mut()
    .context("policy record is missing")?;
    record.pending_signature = Some(pending_signature.to_string());
    record.last_valid_block_height = Some(last_valid_block_height);
    state::save(path, state)?;

    let sent_signature = rpc.send_transaction_with_config(
        &transaction,
        RpcSendTransactionConfig {
            skip_preflight: false,
            preflight_commitment: Some(CommitmentLevel::Finalized),
            ..RpcSendTransactionConfig::default()
        },
    )?;
    if sent_signature != pending_signature {
        bail!("RPC returned a different policy transaction signature");
    }
    rpc.confirm_transaction_with_spinner(
        &sent_signature,
        &blockhash,
        CommitmentConfig::finalized(),
    )?;
    finalize_policy_record(
        rpc,
        path,
        state,
        &plan,
        kind,
        settings,
        deployment.pubkey(),
        delegated.pubkey(),
        sent_signature,
    )
}

#[allow(clippy::too_many_arguments)]
fn finalize_policy_record(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    plan: &kamino::KaminoPlan,
    kind: KaminoPolicyKind,
    settings: Pubkey,
    deployment: Pubkey,
    delegated: Pubkey,
    signature: Signature,
) -> Result<()> {
    let decoded = verify_policy_account(rpc, plan, kind, settings, deployment, delegated)?;
    let finalized_transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let record = policy_record_mut(
        state.kamino.as_mut().context("Kamino record is missing")?,
        kind,
    )
    .as_mut()
    .context("policy record is missing")?;
    record.status = PolicyStatus::Finalized;
    record.creation_signature = Some(signature.to_string());
    record.finalized_slot = Some(finalized_transaction.slot);
    state::save(path, state)?;
    println!(
        "create_{}_policy=PASS policy={} signature={} slot={} start={} rent_collector={}",
        kind.label(),
        plan_policy(plan, kind).0.policy,
        signature,
        finalized_transaction.slot,
        decoded.start,
        decoded.rent_collector
    );
    Ok(())
}

fn verify_policy_account(
    rpc: &RpcClient,
    plan: &kamino::KaminoPlan,
    kind: KaminoPolicyKind,
    settings: Pubkey,
    deployment: Pubkey,
    delegated: Pubkey,
) -> Result<policy::ProgramInteractionPolicyAccount> {
    let (policy_plan, constraints) = plan_policy(plan, kind);
    let account = rpc
        .get_account(&policy_plan.policy)
        .with_context(|| format!("reload {} policy account", kind.label()))?;
    let decoded = policy::decode_program_interaction_policy(account.owner, &account.data)?;
    policy::verify_program_interaction_policy(
        &decoded,
        policy::ExpectedProgramInteractionPolicy {
            policy_address: policy_plan.policy,
            settings,
            seed: kind.seed(),
            delegated_signer: delegated,
            account_index: VAULT_INDEX,
            constraints,
            rent_collector: deployment,
        },
    )?;
    Ok(decoded)
}

fn verify_next_policy_seed(rpc: &RpcClient, settings: Pubkey, expected_seed: u64) -> Result<()> {
    let account = rpc.get_account(&settings)?;
    let settings = squads::decode_settings(&account.data)?;
    let next_seed = settings
        .policy_seed
        .unwrap_or(0)
        .checked_add(1)
        .context("Squads policy seed overflow while deriving the next policy")?;
    if next_seed != expected_seed {
        bail!(
            "next Squads policy seed is {next_seed}, expected {expected_seed}; refusing a mismatched PDA"
        );
    }
    Ok(())
}

fn enforce_policy_prerequisites(state: &VaultState, kind: KaminoPolicyKind) -> Result<()> {
    if kind == KaminoPolicyKind::InitObligation {
        let operations = state
            .kamino
            .as_ref()
            .and_then(|kamino| kamino.operations_policy.as_ref());
        if operations.map(|policy| policy.status) != Some(PolicyStatus::Finalized) {
            bail!("Kamino operations policy must finalize before the init-obligation policy");
        }
    }
    Ok(())
}

fn plan_policy(
    plan: &kamino::KaminoPlan,
    kind: KaminoPolicyKind,
) -> (
    &loyal_actions::autonomous_vaults::KaminoPolicyPlan,
    &[loyal_actions::SquadsInstructionConstraintView],
) {
    match kind {
        KaminoPolicyKind::Operations => (
            &plan.policies.operations,
            plan.operations_constraints.as_slice(),
        ),
        KaminoPolicyKind::InitObligation => (
            &plan.policies.init_obligation,
            plan.init_constraints.as_slice(),
        ),
    }
}

fn policy_record(record: &KaminoRecord, kind: KaminoPolicyKind) -> Option<&PolicyRecord> {
    match kind {
        KaminoPolicyKind::Operations => record.operations_policy.as_ref(),
        KaminoPolicyKind::InitObligation => record.init_obligation_policy.as_ref(),
    }
}

fn policy_record_mut(
    record: &mut KaminoRecord,
    kind: KaminoPolicyKind,
) -> &mut Option<PolicyRecord> {
    match kind {
        KaminoPolicyKind::Operations => &mut record.operations_policy,
        KaminoPolicyKind::InitObligation => &mut record.init_obligation_policy,
    }
}

fn build_policy_transaction(
    rpc: &RpcClient,
    instruction: &Instruction,
    deployment: &solana_sdk::signature::Keypair,
) -> Result<(Transaction, solana_sdk::hash::Hash, u64)> {
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let transaction = Transaction::new_signed_with_payer(
        std::slice::from_ref(instruction),
        Some(&deployment.pubkey()),
        &[deployment],
        blockhash,
    );
    let packet_size = bincode::serialized_size(&transaction).context("measure policy packet")?;
    if packet_size > SOLANA_PACKET_DATA_SIZE {
        bail!(
            "policy transaction is {packet_size} bytes, exceeding Solana's {SOLANA_PACKET_DATA_SIZE}-byte packet limit"
        );
    }
    println!("policy_transaction_packet_bytes={packet_size}");
    Ok((transaction, blockhash, last_valid_block_height))
}

fn simulate_signed_transaction(
    rpc: &RpcClient,
    transaction: &Transaction,
    label: &str,
) -> Result<String> {
    let simulation = rpc.simulate_transaction_with_config(
        transaction,
        RpcSimulateTransactionConfig {
            sig_verify: true,
            replace_recent_blockhash: false,
            commitment: Some(CommitmentConfig::finalized()),
            ..RpcSimulateTransactionConfig::default()
        },
    )?;
    if let Some(error) = simulation.value.err {
        if let Some(logs) = simulation.value.logs {
            for log in logs {
                println!("simulation_log={log}");
            }
        }
        bail!("{label} simulation failed: {error:?}");
    }
    Ok(simulation
        .value
        .units_consumed
        .map(|units| units.to_string())
        .unwrap_or_else(|| "unknown".to_owned()))
}

fn inspect_kamino_execution(
    rpc: &RpcClient,
    state: &VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    let (_, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    require_finalized_kamino_policies(state)?;
    let deployment_usdc = derive_associated_token_address(deployment.pubkey(), USDC_MINT);
    let deployment_usdc_amount =
        token_account_amount(rpc, deployment_usdc, deployment.pubkey(), USDC_MINT)?.unwrap_or(0);
    let vault_usdc_amount =
        token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?.unwrap_or(0);
    let metadata = derive_kamino_user_metadata(vault);
    let metadata_exists = account_exists_with_owner(rpc, metadata, KAMINO_LEND_PROGRAM_ID)?;

    println!("module=kamino-execution-readiness verdict=PASS");
    println!("deployment_usdc_token_account={deployment_usdc}");
    println!("deployment_usdc_raw={deployment_usdc_amount}");
    println!("vault_lamports={}", rpc.get_balance(&vault)?);
    println!("vault_usdc_token_account={}", plan.vault_usdc);
    println!("vault_usdc_raw={vault_usdc_amount}");
    println!("kamino_user_metadata={metadata}");
    println!("kamino_user_metadata_exists={metadata_exists}");
    for (index, reserve) in plan.reserves.iter().enumerate() {
        let obligation = kamino::load_obligation_snapshot(rpc, vault, reserve)?;
        let farm_exists = match reserve.obligation_farm_user_state {
            Some(farm) => account_exists_with_owner(rpc, farm, KAMINO_FARMS_PROGRAM_ID)?,
            None => false,
        };
        println!(
            "kamino_obligation index={} address={} exists={} lamports={} deposited_raw={} farm_user={} farm_exists={}",
            index,
            reserve.obligation,
            obligation.exists,
            obligation.lamports,
            obligation.deposited_amount,
            reserve
                .obligation_farm_user_state
                .map(|key| key.to_string())
                .as_deref()
                .unwrap_or("none"),
            farm_exists
        );
    }
    for step in &state
        .kamino
        .as_ref()
        .context("Kamino state record is missing")?
        .live_steps
    {
        println!(
            "kamino_live_step name={} status={:?} signature={} slot={}",
            step.name,
            step.status,
            step.finalized_signature.as_deref().unwrap_or("pending"),
            step.finalized_slot
                .map(|slot| slot.to_string())
                .as_deref()
                .unwrap_or("pending")
        );
    }
    Ok(())
}

fn setup_kamino_accounts(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    const STEP: &str = "kamino-account-setup";
    let (settings, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    require_finalized_kamino_policies(state)?;
    let deployment_usdc = derive_associated_token_address(deployment.pubkey(), USDC_MINT);
    let before =
        kamino_setup_observations(rpc, deployment.pubkey(), deployment_usdc, vault, &plan)?;
    ensure_live_step(path, state, STEP, before.clone())?;
    let before = live_step(state, STEP)?.before.clone();

    if let Some(signature) = recover_finalized_live_step(rpc, state, STEP)? {
        let after = verify_kamino_setup(
            rpc,
            deployment.pubkey(),
            deployment_usdc,
            vault,
            &plan,
            &before,
        )?;
        return finalize_live_step(rpc, path, state, STEP, signature, after);
    }
    if live_step(state, STEP)?.status == PolicyStatus::Finalized {
        verify_kamino_setup(
            rpc,
            deployment.pubkey(),
            deployment_usdc,
            vault,
            &plan,
            &live_step(state, STEP)?.before,
        )?;
        println!("{STEP}=PASS already_finalized=true");
        return Ok(());
    }

    let deployment_usdc_before = *before.get("deployment_usdc_raw").unwrap_or(&0);
    if deployment_usdc_before < KAMINO_TEST_USDC_RAW {
        bail!(
            "deployment USDC balance is {deployment_usdc_before}, below the {KAMINO_TEST_USDC_RAW}-raw test budget"
        );
    }
    if *before.get("vault_usdc_exists").unwrap_or(&0) != 0
        || *before.get("user_metadata_exists").unwrap_or(&0) != 0
    {
        bail!("Kamino setup accounts already exist without finalized setup evidence");
    }

    let inner_instructions = kamino::setup_inner_instructions(vault, plan.vault_usdc);
    let mut transaction_accounts = Vec::new();
    let compiled = inner_instructions
        .into_iter()
        .map(|instruction| compile_squads_inner_instruction(&mut transaction_accounts, instruction))
        .collect();
    let settings_setup = execute_sync_transaction_instruction(
        settings,
        deployment.pubkey(),
        VAULT_INDEX,
        compiled,
        transaction_accounts,
    );
    let fund_vault =
        system_instruction::transfer(&deployment.pubkey(), &vault, KAMINO_SETUP_VAULT_LAMPORTS);
    let fund_usdc = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &deployment_usdc,
        &USDC_MINT,
        &plan.vault_usdc,
        &deployment.pubkey(),
        &[],
        KAMINO_TEST_USDC_RAW,
        6,
    )?;
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &[fund_vault, settings_setup, fund_usdc], deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, STEP)?;
    println!("{STEP}_simulation=PASS units_consumed={units}");
    let signature = send_live_step_transaction(
        rpc,
        path,
        state,
        STEP,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_kamino_setup(
        rpc,
        deployment.pubkey(),
        deployment_usdc,
        vault,
        &plan,
        &before,
    )?;
    finalize_live_step(rpc, path, state, STEP, signature, after)
}

fn parse_reserve_index() -> Result<usize> {
    let value = env::args()
        .nth(2)
        .context("missing Kamino reserve index; expected 0 or 1")?;
    let index = value
        .parse::<usize>()
        .context("Kamino reserve index must be an integer")?;
    if index > 1 {
        bail!("Kamino reserve index must be 0 or 1");
    }
    Ok(index)
}

fn init_kamino_obligation(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    reserve_index: usize,
) -> Result<()> {
    let step_name = format!("kamino-init-obligation-{reserve_index}");
    init_kamino_obligation_step(
        rpc,
        path,
        state,
        deployment,
        delegated,
        reserve_index,
        &step_name,
        "kamino-account-setup",
    )
}

fn reinit_kamino_obligation(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    reserve_index: usize,
) -> Result<()> {
    let step_name = format!("kamino-reinit-obligation-{reserve_index}");
    let prerequisite = KaminoOperationKind::FullWithdraw.step_name(reserve_index);
    init_kamino_obligation_step(
        rpc,
        path,
        state,
        deployment,
        delegated,
        reserve_index,
        &step_name,
        &prerequisite,
    )
}

#[allow(clippy::too_many_arguments)]
fn init_kamino_obligation_step(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    reserve_index: usize,
    step_name: &str,
    prerequisite: &str,
) -> Result<()> {
    let (settings, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    require_finalized_kamino_policies(state)?;
    require_finalized_live_step(state, prerequisite)?;
    let reserve = plan
        .reserves
        .get(reserve_index)
        .context("Kamino reserve index is outside the approved manifest")?;
    let before = kamino_obligation_observations(rpc, vault, &plan, reserve_index)?;
    ensure_live_step(path, state, step_name, before)?;
    let before = live_step(state, step_name)?.before.clone();

    if let Some(signature) = recover_finalized_live_step(rpc, state, step_name)? {
        let after = verify_kamino_obligation_init(rpc, vault, &plan, reserve_index, &before)?;
        return finalize_live_step(rpc, path, state, step_name, signature, after);
    }
    if live_step(state, step_name)?.status == PolicyStatus::Finalized {
        verify_kamino_obligation_init(rpc, vault, &plan, reserve_index, &before)?;
        println!("{step_name}=PASS already_finalized=true");
        return Ok(());
    }
    if before.get("obligation_exists") != Some(&0) {
        bail!("approved Kamino obligation existed before its delegated init evidence");
    }

    let policy = state
        .kamino
        .as_ref()
        .and_then(|record| record.init_obligation_policy.as_ref())
        .context("Kamino init-obligation policy record is missing")?;
    let policy_address = Pubkey::from_str(&policy.policy)?;
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(
        &mut transaction_accounts,
        reserve.init_obligation_instruction.clone(),
    );
    let execute = execute_program_interaction_policy_instruction(
        policy_address,
        delegated.pubkey(),
        VAULT_INDEX,
        vec![compiled],
        vec![0],
        transaction_accounts,
    );
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &[execute], delegated)?;
    let units = simulate_signed_transaction(rpc, &transaction, step_name)?;
    println!("{step_name}_simulation=PASS units_consumed={units}");
    println!("policy_execution_path={policy_address}");
    println!("policy_signer={}", delegated.pubkey());
    println!("settings_setup_path_used=false settings={settings}");
    let signature = send_live_step_transaction(
        rpc,
        path,
        state,
        step_name,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_kamino_obligation_init(rpc, vault, &plan, reserve_index, &before)?;
    finalize_live_step(rpc, path, state, step_name, signature, after)
}

fn setup_kamino_farms(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
) -> Result<()> {
    const STEP: &str = "kamino-farm-setup";
    let (settings, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    require_finalized_kamino_policies(state)?;
    require_finalized_live_step(state, "kamino-init-obligation-0")?;
    require_finalized_live_step(state, "kamino-init-obligation-1")?;
    let before = kamino_farm_observations(rpc, vault, &plan)?;
    ensure_live_step(path, state, STEP, before)?;
    let before = live_step(state, STEP)?.before.clone();

    if let Some(signature) = recover_finalized_live_step(rpc, state, STEP)? {
        let after = verify_kamino_farm_setup(rpc, vault, &plan, &before)?;
        return finalize_live_step(rpc, path, state, STEP, signature, after);
    }
    if live_step(state, STEP)?.status == PolicyStatus::Finalized {
        verify_kamino_farm_setup(rpc, vault, &plan, &before)?;
        println!("{STEP}=PASS already_finalized=true");
        return Ok(());
    }
    for index in 0..plan.reserves.len() {
        if before.get(&format!("farm_{index}_exists")) != Some(&0) {
            bail!("Kamino farm user {index} existed before its setup evidence");
        }
        let obligation = kamino::load_obligation_snapshot(rpc, vault, &plan.reserves[index])?;
        if !obligation.exists || obligation.deposited_amount != 0 {
            bail!("Kamino obligation {index} is not a fresh initialized obligation");
        }
    }

    let mut transaction_accounts = Vec::new();
    let compiled = kamino::farm_setup_inner_instructions(vault, &plan)?
        .into_iter()
        .map(|instruction| compile_squads_inner_instruction(&mut transaction_accounts, instruction))
        .collect();
    let execute = execute_sync_transaction_instruction(
        settings,
        deployment.pubkey(),
        VAULT_INDEX,
        compiled,
        transaction_accounts,
    );
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &[execute], deployment)?;
    let units = simulate_signed_transaction(rpc, &transaction, STEP)?;
    println!("{STEP}_simulation=PASS units_consumed={units}");
    println!(
        "setup_exception_path=settings signer={}",
        deployment.pubkey()
    );
    println!("inner_rent_payer={vault}");
    let signature = send_live_step_transaction(
        rpc,
        path,
        state,
        STEP,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_kamino_farm_setup(rpc, vault, &plan, &before)?;
    finalize_live_step(rpc, path, state, STEP, signature, after)
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum KaminoOperationKind {
    Deposit,
    PartialWithdraw,
    FullWithdraw,
}

impl KaminoOperationKind {
    fn step_name(self, reserve_index: usize) -> String {
        let operation = match self {
            Self::Deposit => "deposit",
            Self::PartialWithdraw => "partial-withdraw",
            Self::FullWithdraw => "full-withdraw",
        };
        format!("kamino-{operation}-{reserve_index}")
    }

    fn constraint_index(self) -> u8 {
        match self {
            Self::Deposit => 0,
            Self::PartialWithdraw | Self::FullWithdraw => 1,
        }
    }

    fn requested_amount(self, before: &BTreeMap<String, u64>) -> Result<u64> {
        match self {
            Self::Deposit => Ok(KAMINO_SINGLE_RESERVE_TEST_USDC_RAW),
            Self::PartialWithdraw => {
                let deposited = before
                    .get("obligation_deposited_raw")
                    .copied()
                    .context("missing deposited collateral before partial withdrawal")?;
                let amount = deposited / 2;
                if amount == 0 || amount == deposited {
                    bail!("deposited collateral is too small for a nonzero partial withdrawal");
                }
                Ok(amount)
            }
            Self::FullWithdraw => Ok(u64::MAX),
        }
    }
}

fn execute_kamino_operation(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    deployment: &solana_sdk::signature::Keypair,
    delegated: &solana_sdk::signature::Keypair,
    reserve_index: usize,
    operation: KaminoOperationKind,
) -> Result<()> {
    let step_name = operation.step_name(reserve_index);
    let (_, vault, plan) = load_kamino_plan(rpc, state, deployment, delegated)?;
    require_finalized_kamino_policies(state)?;
    require_kamino_operation_prerequisites(state, reserve_index, operation)?;
    let reserve = plan
        .reserves
        .get(reserve_index)
        .context("Kamino reserve index is outside the approved manifest")?;
    let mut before = kamino_operation_observations(rpc, vault, &plan, reserve_index)?;
    let requested_amount = operation.requested_amount(&before)?;
    before.insert("requested_amount_raw".to_owned(), requested_amount);
    ensure_live_step(path, state, &step_name, before)?;
    let before = live_step(state, &step_name)?.before.clone();
    let requested_amount = before
        .get("requested_amount_raw")
        .copied()
        .context("recorded Kamino operation is missing its requested amount")?;

    if let Some(signature) = recover_finalized_live_step(rpc, state, &step_name)? {
        let after = verify_kamino_operation(
            rpc,
            vault,
            &plan,
            reserve_index,
            operation,
            requested_amount,
            &before,
        )?;
        return finalize_live_step(rpc, path, state, &step_name, signature, after);
    }
    if live_step(state, &step_name)?.status == PolicyStatus::Finalized {
        verify_kamino_operation(
            rpc,
            vault,
            &plan,
            reserve_index,
            operation,
            requested_amount,
            &before,
        )?;
        println!("{step_name}=PASS already_finalized=true");
        return Ok(());
    }
    validate_kamino_operation_before(operation, &before)?;

    let inner = match operation {
        KaminoOperationKind::Deposit => {
            kamino::instruction_with_amount(&reserve.deposit_instruction, requested_amount)?
        }
        KaminoOperationKind::PartialWithdraw | KaminoOperationKind::FullWithdraw => {
            kamino::instruction_with_amount(&reserve.withdraw_instruction, requested_amount)?
        }
    };
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, inner);
    let operations_policy = state
        .kamino
        .as_ref()
        .and_then(|record| record.operations_policy.as_ref())
        .context("Kamino operations policy record is missing")?;
    let policy_address = Pubkey::from_str(&operations_policy.policy)?;
    let policy_execute = execute_program_interaction_policy_instruction(
        policy_address,
        delegated.pubkey(),
        VAULT_INDEX,
        vec![compiled],
        vec![operation.constraint_index()],
        transaction_accounts,
    );
    let mut instructions = vec![ComputeBudgetInstruction::request_heap_frame(
        SQUADS_EXTENDED_HEAP_FRAME_BYTES,
    )];
    instructions.extend(kamino::refresh_instructions(rpc, vault, reserve)?);
    instructions.push(policy_execute);
    let (transaction, blockhash, last_valid_block_height) =
        build_signed_transaction(rpc, &instructions, delegated)?;
    let units = simulate_signed_transaction(rpc, &transaction, &step_name)?;
    println!("{step_name}_simulation=PASS units_consumed={units}");
    println!("policy_execution_path={policy_address}");
    println!("policy_signer={}", delegated.pubkey());
    println!("constraint_index={}", operation.constraint_index());
    println!("requested_amount_raw={requested_amount}");
    let signature = send_live_step_transaction(
        rpc,
        path,
        state,
        &step_name,
        transaction,
        blockhash,
        last_valid_block_height,
    )?;
    let after = verify_kamino_operation(
        rpc,
        vault,
        &plan,
        reserve_index,
        operation,
        requested_amount,
        &before,
    )?;
    finalize_live_step(rpc, path, state, &step_name, signature, after)
}

fn require_kamino_operation_prerequisites(
    state: &VaultState,
    reserve_index: usize,
    operation: KaminoOperationKind,
) -> Result<()> {
    require_finalized_live_step(state, "kamino-farm-setup")?;
    let prerequisite = match operation {
        KaminoOperationKind::Deposit if reserve_index == 1 => {
            Some("kamino-reinit-obligation-0".to_owned())
        }
        KaminoOperationKind::Deposit => None,
        KaminoOperationKind::PartialWithdraw => {
            Some(KaminoOperationKind::Deposit.step_name(reserve_index))
        }
        KaminoOperationKind::FullWithdraw => {
            Some(KaminoOperationKind::PartialWithdraw.step_name(reserve_index))
        }
    };
    if let Some(prerequisite) = prerequisite {
        require_finalized_live_step(state, &prerequisite)?;
    }
    Ok(())
}

fn validate_kamino_operation_before(
    operation: KaminoOperationKind,
    before: &BTreeMap<String, u64>,
) -> Result<()> {
    if before.get("obligation_exists") != Some(&1) || before.get("farm_exists") != Some(&1) {
        bail!("Kamino obligation and persistent farm must exist before protected execution");
    }
    let deposited = before
        .get("obligation_deposited_raw")
        .copied()
        .context("missing before deposited collateral")?;
    match operation {
        KaminoOperationKind::Deposit if deposited != 0 => {
            bail!("Kamino deposit canary requires an empty obligation")
        }
        KaminoOperationKind::PartialWithdraw | KaminoOperationKind::FullWithdraw
            if deposited == 0 =>
        {
            bail!("Kamino withdrawal canary requires deposited collateral")
        }
        _ => Ok(()),
    }
}

fn require_finalized_live_step(state: &VaultState, name: &str) -> Result<()> {
    if live_step(state, name)?.status != PolicyStatus::Finalized {
        bail!("live prerequisite {name} must be finalized first");
    }
    Ok(())
}

fn kamino_obligation_observations(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
    reserve_index: usize,
) -> Result<BTreeMap<String, u64>> {
    let reserve = plan
        .reserves
        .get(reserve_index)
        .context("Kamino reserve index is outside the approved manifest")?;
    let obligation = kamino::load_obligation_snapshot(rpc, vault, reserve)?;
    let mut observations = BTreeMap::new();
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    observations.insert(
        "vault_usdc_raw".to_owned(),
        token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?
            .context("vault USDC account is absent")?,
    );
    observations.insert("obligation_exists".to_owned(), u64::from(obligation.exists));
    observations.insert("obligation_lamports".to_owned(), obligation.lamports);
    observations.insert(
        "obligation_deposited_raw".to_owned(),
        obligation.deposited_amount,
    );
    Ok(observations)
}

fn verify_kamino_obligation_init(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
    reserve_index: usize,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = kamino_obligation_observations(rpc, vault, plan, reserve_index)?;
    let before_vault = before
        .get("vault_lamports")
        .copied()
        .context("missing before vault lamports")?;
    let after_vault = after
        .get("vault_lamports")
        .copied()
        .context("missing after vault lamports")?;
    let obligation_lamports = after
        .get("obligation_lamports")
        .copied()
        .context("missing after obligation lamports")?;
    if after.get("obligation_exists") != Some(&1)
        || after.get("obligation_deposited_raw") != Some(&0)
        || obligation_lamports == 0
        || before_vault.checked_sub(obligation_lamports) != Some(after_vault)
        || before.get("vault_usdc_raw") != after.get("vault_usdc_raw")
    {
        bail!("delegated Kamino obligation init does not match the exact RPC delta manifest");
    }
    Ok(after)
}

fn kamino_farm_observations(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
) -> Result<BTreeMap<String, u64>> {
    let mut observations = BTreeMap::new();
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    for (index, reserve) in plan.reserves.iter().enumerate() {
        let address = reserve
            .obligation_farm_user_state
            .context("approved Kamino reserve has no farm-user PDA")?;
        let account = rpc.get_account_with_commitment(&address, CommitmentConfig::finalized())?;
        match account.value {
            Some(account) => {
                if account.owner != KAMINO_FARMS_PROGRAM_ID {
                    bail!("Kamino farm user {address} has an unexpected owner");
                }
                observations.insert(format!("farm_{index}_exists"), 1);
                observations.insert(format!("farm_{index}_lamports"), account.lamports);
                observations.insert(format!("farm_{index}_data_len"), account.data.len() as u64);
            }
            None => {
                observations.insert(format!("farm_{index}_exists"), 0);
                observations.insert(format!("farm_{index}_lamports"), 0);
                observations.insert(format!("farm_{index}_data_len"), 0);
            }
        }
    }
    Ok(observations)
}

fn verify_kamino_farm_setup(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = kamino_farm_observations(rpc, vault, plan)?;
    let mut total_farm_rent = 0_u64;
    for index in 0..plan.reserves.len() {
        if after.get(&format!("farm_{index}_exists")) != Some(&1)
            || after.get(&format!("farm_{index}_data_len")) != Some(&KFARMS_USER_STATE_BYTES)
        {
            bail!("Kamino farm user {index} does not match the persistent 920-byte manifest");
        }
        total_farm_rent = total_farm_rent
            .checked_add(
                after
                    .get(&format!("farm_{index}_lamports"))
                    .copied()
                    .context("missing farm rent observation")?,
            )
            .context("farm rent sum overflow")?;
        let obligation = kamino::load_obligation_snapshot(rpc, vault, &plan.reserves[index])?;
        if !obligation.exists || obligation.deposited_amount != 0 {
            bail!("farm setup changed or removed Kamino obligation {index}");
        }
    }
    let before_vault = before
        .get("vault_lamports")
        .copied()
        .context("missing before farm-setup vault lamports")?;
    let after_vault = after
        .get("vault_lamports")
        .copied()
        .context("missing after farm-setup vault lamports")?;
    if before_vault.checked_sub(total_farm_rent) != Some(after_vault) {
        bail!("Kamino farm rent was not paid exactly by the autonomous vault");
    }
    Ok(after)
}

fn kamino_operation_observations(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
    reserve_index: usize,
) -> Result<BTreeMap<String, u64>> {
    let reserve = plan
        .reserves
        .get(reserve_index)
        .context("Kamino reserve index is outside the approved manifest")?;
    let obligation = kamino::load_obligation_snapshot(rpc, vault, reserve)?;
    let farm_address = reserve
        .obligation_farm_user_state
        .context("approved Kamino reserve has no farm-user PDA")?;
    let farm = rpc.get_account_with_commitment(&farm_address, CommitmentConfig::finalized())?;
    let (farm_exists, farm_lamports, farm_data_len) = match farm.value {
        Some(account) => {
            if account.owner != KAMINO_FARMS_PROGRAM_ID {
                bail!("Kamino farm user {farm_address} has an unexpected owner");
            }
            (1, account.lamports, account.data.len() as u64)
        }
        None => (0, 0, 0),
    };
    let mut observations = BTreeMap::new();
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    observations.insert(
        "vault_usdc_raw".to_owned(),
        token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?
            .context("vault USDC account is absent")?,
    );
    observations.insert("obligation_exists".to_owned(), u64::from(obligation.exists));
    observations.insert("obligation_lamports".to_owned(), obligation.lamports);
    observations.insert(
        "obligation_deposited_raw".to_owned(),
        obligation.deposited_amount,
    );
    observations.insert("farm_exists".to_owned(), farm_exists);
    observations.insert("farm_lamports".to_owned(), farm_lamports);
    observations.insert("farm_data_len".to_owned(), farm_data_len);
    Ok(observations)
}

fn verify_kamino_operation(
    rpc: &RpcClient,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
    reserve_index: usize,
    operation: KaminoOperationKind,
    requested_amount: u64,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = kamino_operation_observations(rpc, vault, plan, reserve_index)?;
    if after.get("farm_exists") != Some(&1)
        || after.get("farm_data_len") != Some(&KFARMS_USER_STATE_BYTES)
        || before.get("farm_lamports") != after.get("farm_lamports")
    {
        bail!("protected Kamino operation changed or removed its persistent farm user");
    }
    let before_usdc = before
        .get("vault_usdc_raw")
        .copied()
        .context("missing before vault USDC")?;
    let after_usdc = after
        .get("vault_usdc_raw")
        .copied()
        .context("missing after vault USDC")?;
    let before_deposited = before
        .get("obligation_deposited_raw")
        .copied()
        .context("missing before deposited collateral")?;
    let after_deposited = after
        .get("obligation_deposited_raw")
        .copied()
        .context("missing after deposited collateral")?;
    let before_vault_lamports = before
        .get("vault_lamports")
        .copied()
        .context("missing before vault lamports")?;
    let after_vault_lamports = after
        .get("vault_lamports")
        .copied()
        .context("missing after vault lamports")?;
    let before_obligation_lamports = before
        .get("obligation_lamports")
        .copied()
        .context("missing before obligation lamports")?;

    match operation {
        KaminoOperationKind::Deposit => {
            if before_usdc.checked_sub(requested_amount) != Some(after_usdc)
                || after.get("obligation_exists") != Some(&1)
                || after_deposited == 0
                || after.get("obligation_lamports") != Some(&before_obligation_lamports)
                || after_vault_lamports != before_vault_lamports
            {
                bail!("delegated Kamino deposit does not match the exact RPC delta manifest");
            }
        }
        KaminoOperationKind::PartialWithdraw => {
            if before_deposited.checked_sub(requested_amount) != Some(after_deposited)
                || after_usdc <= before_usdc
                || after.get("obligation_exists") != Some(&1)
                || after.get("obligation_lamports") != Some(&before_obligation_lamports)
                || after_vault_lamports != before_vault_lamports
            {
                bail!(
                    "delegated Kamino partial withdrawal does not match the exact RPC delta manifest"
                );
            }
        }
        KaminoOperationKind::FullWithdraw => {
            if after_deposited != 0 || after_usdc <= before_usdc {
                bail!("delegated Kamino full withdrawal did not return liquidity to the vault");
            }
            match after.get("obligation_exists") {
                Some(1) => {
                    if after.get("obligation_lamports") != Some(&before_obligation_lamports)
                        || after_vault_lamports != before_vault_lamports
                    {
                        bail!("persistent Kamino obligation changed rent on full withdrawal");
                    }
                }
                Some(0) => {
                    if after.get("obligation_lamports") != Some(&0)
                        || before_vault_lamports.checked_add(before_obligation_lamports)
                            != Some(after_vault_lamports)
                    {
                        bail!("closed Kamino obligation did not refund exact rent to the vault");
                    }
                }
                _ => bail!("invalid full-withdraw obligation existence observation"),
            }
        }
    }
    Ok(after)
}

fn require_finalized_kamino_policies(state: &VaultState) -> Result<()> {
    let kamino = state
        .kamino
        .as_ref()
        .context("Kamino state record is missing")?;
    if kamino
        .operations_policy
        .as_ref()
        .map(|policy| policy.status)
        != Some(PolicyStatus::Finalized)
        || kamino
            .init_obligation_policy
            .as_ref()
            .map(|policy| policy.status)
            != Some(PolicyStatus::Finalized)
    {
        bail!("both Kamino policies must finalize before delegated execution setup");
    }
    Ok(())
}

fn kamino_setup_observations(
    rpc: &RpcClient,
    deployment: Pubkey,
    deployment_usdc: Pubkey,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
) -> Result<BTreeMap<String, u64>> {
    let mut observations = BTreeMap::new();
    observations.insert(
        "deployment_lamports".to_owned(),
        rpc.get_balance(&deployment)?,
    );
    observations.insert(
        "deployment_usdc_raw".to_owned(),
        token_account_amount(rpc, deployment_usdc, deployment, USDC_MINT)?.unwrap_or(0),
    );
    observations.insert("vault_lamports".to_owned(), rpc.get_balance(&vault)?);
    let vault_usdc = token_account_amount(rpc, plan.vault_usdc, vault, USDC_MINT)?;
    observations.insert(
        "vault_usdc_exists".to_owned(),
        u64::from(vault_usdc.is_some()),
    );
    observations.insert("vault_usdc_raw".to_owned(), vault_usdc.unwrap_or(0));
    observations.insert(
        "user_metadata_exists".to_owned(),
        u64::from(account_exists_with_owner(
            rpc,
            derive_kamino_user_metadata(vault),
            KAMINO_LEND_PROGRAM_ID,
        )?),
    );
    Ok(observations)
}

fn verify_kamino_setup(
    rpc: &RpcClient,
    deployment: Pubkey,
    deployment_usdc: Pubkey,
    vault: Pubkey,
    plan: &kamino::KaminoPlan,
    before: &BTreeMap<String, u64>,
) -> Result<BTreeMap<String, u64>> {
    let after = kamino_setup_observations(rpc, deployment, deployment_usdc, vault, plan)?;
    let deployment_before = *before
        .get("deployment_usdc_raw")
        .context("missing before USDC")?;
    let deployment_after = *after
        .get("deployment_usdc_raw")
        .context("missing after USDC")?;
    if deployment_before.checked_sub(KAMINO_TEST_USDC_RAW) != Some(deployment_after)
        || after.get("vault_usdc_exists") != Some(&1)
        || after.get("vault_usdc_raw") != Some(&KAMINO_TEST_USDC_RAW)
        || after.get("user_metadata_exists") != Some(&1)
        || after.get("vault_lamports").copied().unwrap_or(0) == 0
    {
        bail!("finalized Kamino setup balances/accounts do not match the exact setup manifest");
    }
    Ok(after)
}

fn derive_associated_token_address(owner: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), spl_token::id().as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn token_account_amount(
    rpc: &RpcClient,
    address: Pubkey,
    expected_owner: Pubkey,
    expected_mint: Pubkey,
) -> Result<Option<u64>> {
    let response = rpc.get_account_with_commitment(&address, CommitmentConfig::finalized())?;
    let Some(account) = response.value else {
        return Ok(None);
    };
    if account.owner != spl_token::id() {
        bail!("token account {address} has an unexpected program owner");
    }
    let token = spl_token::state::Account::unpack(&account.data)
        .with_context(|| format!("decode token account {address}"))?;
    if token.owner != expected_owner || token.mint != expected_mint {
        bail!("token account {address} owner or mint does not match the manifest");
    }
    Ok(Some(token.amount))
}

fn account_exists_with_owner(rpc: &RpcClient, address: Pubkey, owner: Pubkey) -> Result<bool> {
    let response = rpc.get_account_with_commitment(&address, CommitmentConfig::finalized())?;
    match response.value {
        Some(account) if account.owner == owner => Ok(true),
        Some(account) => bail!(
            "account {address} is owned by {}, expected {owner}",
            account.owner
        ),
        None => Ok(false),
    }
}

fn ensure_live_step(
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    before: BTreeMap<String, u64>,
) -> Result<()> {
    let steps = &mut state
        .kamino
        .as_mut()
        .context("Kamino state record is missing")?
        .live_steps;
    if steps.iter().all(|step| step.name != name) {
        steps.push(LiveStepRecord {
            name: name.to_owned(),
            status: PolicyStatus::Planned,
            pending_signature: None,
            last_valid_block_height: None,
            finalized_signature: None,
            finalized_slot: None,
            before,
            after: BTreeMap::new(),
        });
        state::save(path, state)?;
    }
    Ok(())
}

fn live_step<'a>(state: &'a VaultState, name: &str) -> Result<&'a LiveStepRecord> {
    state
        .kamino
        .as_ref()
        .and_then(|kamino| kamino.live_steps.iter().find(|step| step.name == name))
        .with_context(|| format!("live step {name} is missing"))
}

fn live_step_mut<'a>(state: &'a mut VaultState, name: &str) -> Result<&'a mut LiveStepRecord> {
    state
        .kamino
        .as_mut()
        .and_then(|kamino| kamino.live_steps.iter_mut().find(|step| step.name == name))
        .with_context(|| format!("live step {name} is missing"))
}

fn recover_finalized_live_step(
    rpc: &RpcClient,
    state: &VaultState,
    name: &str,
) -> Result<Option<Signature>> {
    let step = live_step(state, name)?;
    if step.status == PolicyStatus::Finalized {
        return Ok(None);
    }
    let Some(signature) = step.pending_signature.as_deref() else {
        return Ok(None);
    };
    let signature = Signature::from_str(signature)?;
    let statuses = rpc.get_signature_statuses(&[signature])?;
    if let Some(status) = statuses.value.into_iter().next().flatten() {
        if let Some(error) = status.err {
            bail!("recorded {name} transaction failed on chain: {error:?}");
        }
        if status.satisfies_commitment(CommitmentConfig::finalized()) {
            return Ok(Some(signature));
        }
        bail!("recorded {name} transaction is not finalized yet");
    }
    let current = rpc.get_block_height()?;
    let last_valid = step
        .last_valid_block_height
        .context("pending live step is missing last valid block height")?;
    if current <= last_valid {
        bail!("recorded {name} signature is still live but not visible; retry later");
    }
    Ok(None)
}

fn build_signed_transaction(
    rpc: &RpcClient,
    instructions: &[Instruction],
    signer: &solana_sdk::signature::Keypair,
) -> Result<(Transaction, solana_sdk::hash::Hash, u64)> {
    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let transaction = Transaction::new_signed_with_payer(
        instructions,
        Some(&signer.pubkey()),
        &[signer],
        blockhash,
    );
    let packet_size = bincode::serialized_size(&transaction)?;
    if packet_size > SOLANA_PACKET_DATA_SIZE {
        bail!(
            "transaction is {packet_size} bytes, exceeding Solana's {SOLANA_PACKET_DATA_SIZE}-byte packet limit"
        );
    }
    println!("transaction_packet_bytes={packet_size}");
    Ok((transaction, blockhash, last_valid_block_height))
}

fn send_live_step_transaction(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    transaction: Transaction,
    blockhash: solana_sdk::hash::Hash,
    last_valid_block_height: u64,
) -> Result<Signature> {
    let pending_signature = transaction.signatures[0];
    let step = live_step_mut(state, name)?;
    step.pending_signature = Some(pending_signature.to_string());
    step.last_valid_block_height = Some(last_valid_block_height);
    state::save(path, state)?;
    let sent_signature = rpc.send_transaction_with_config(
        &transaction,
        RpcSendTransactionConfig {
            skip_preflight: false,
            preflight_commitment: Some(CommitmentLevel::Finalized),
            ..RpcSendTransactionConfig::default()
        },
    )?;
    if sent_signature != pending_signature {
        bail!("RPC returned a different transaction signature for {name}");
    }
    rpc.confirm_transaction_with_spinner(
        &sent_signature,
        &blockhash,
        CommitmentConfig::finalized(),
    )?;
    Ok(sent_signature)
}

fn finalize_live_step(
    rpc: &RpcClient,
    path: &std::path::PathBuf,
    state: &mut VaultState,
    name: &str,
    signature: Signature,
    after: BTreeMap<String, u64>,
) -> Result<()> {
    let transaction = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: None,
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let step = live_step_mut(state, name)?;
    step.status = PolicyStatus::Finalized;
    step.finalized_signature = Some(signature.to_string());
    step.finalized_slot = Some(transaction.slot);
    step.after = after;
    state::save(path, state)?;
    println!(
        "{name}=PASS signature={signature} slot={}",
        transaction.slot
    );
    Ok(())
}
