use std::{fs, path::PathBuf, str::FromStr};

use anyhow::Context;
use balance_sweep_ata_monitor::smart_account::{EARN_AUTODEPOSIT_WALLET_ATAS, EARN_WALLETS};
use balance_sweep_ata_monitor::{
    enqueue_normalized_earn_update, normalize_laserstream_update,
    process_next_autodeposit_reconciliation_request,
    process_next_earn_reconciliation_job_with_policy_monitor, subscribe_request_json,
    AutodepositReconciliationProcessOutcome, EarnReconciliationProcessOutcome, RpcEarnChainReader,
    SubscriptionWatchSet, EARN_SMART_ACCOUNTS,
};
use clap::Parser;
use helius_laserstream::grpc::{
    subscribe_update::UpdateOneof, SubscribeUpdate, SubscribeUpdateAccount,
    SubscribeUpdateAccountInfo,
};
use loyal_squads_policy_monitor::{
    Cluster, Commitment, MonitorConfig, PolicyMonitor, PostgresPolicyMatchSink,
};
use loyal_yield_store::{
    EarnSubscriptionTarget, OrchestratorConfig, OrchestratorStore, PolicyMatchInput,
};
use serde::Deserialize;
use serde_json::json;
use solana_sdk::{pubkey::Pubkey, signature::Signature};
use tokio::sync::Mutex;

#[derive(Debug, Parser)]
struct Args {
    #[arg(long)]
    postgres_url: String,
    #[arg(long)]
    rpc_url: String,
    #[arg(long)]
    state: PathBuf,
    #[arg(long)]
    transactions: PathBuf,
    #[arg(long)]
    subscribe_request_output: PathBuf,
    #[arg(long)]
    pending_floor_ready: PathBuf,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LocalState {
    policy_account: String,
    recurring_delegation: String,
    settings_pda: String,
    subscription_authority: String,
    vault_pubkey: String,
    wallet_address: String,
    wallet_usdc_ata: String,
}

#[derive(Debug, Clone, Copy, Deserialize)]
#[serde(rename_all = "snake_case")]
enum SetupStage {
    ApproveTokenDelegate,
    CloseAutodeposit,
    CreatePolicy,
    CreateRecurringDelegation,
    InitializeSubscriptionAuthority,
}

#[derive(Debug, Clone, Deserialize)]
struct ChainTransaction {
    signature: String,
    slot: u64,
    stage: SetupStage,
}

fn initial_watch_set(state: &LocalState) -> anyhow::Result<SubscriptionWatchSet> {
    SubscriptionWatchSet::from_targets(
        Vec::new(),
        vec![EarnSubscriptionTarget {
            environment: "mainnet-beta".to_owned(),
            settings: state.settings_pda.clone(),
            wallet: state.wallet_address.clone(),
            earn_max: false,
            vault_index: 1,
            vault_pubkey: Some(state.vault_pubkey.clone()),
            policy_accounts: Vec::new(),
            markets: Vec::new(),
            autodeposit_accounts: Vec::new(),
            observation_start_slot: None,
        }],
    )
}

fn emulated_update(
    state: &LocalState,
    transaction: ChainTransaction,
) -> anyhow::Result<SubscribeUpdate> {
    let (filter, account_pubkey, lamports) = match transaction.stage {
        SetupStage::InitializeSubscriptionAuthority => {
            (EARN_WALLETS, state.wallet_address.clone(), 1)
        }
        SetupStage::CloseAutodeposit => (EARN_SMART_ACCOUNTS, state.policy_account.clone(), 0),
        SetupStage::CreatePolicy => (EARN_SMART_ACCOUNTS, state.settings_pda.clone(), 1),
        SetupStage::CreateRecurringDelegation => (
            EARN_AUTODEPOSIT_WALLET_ATAS,
            state.wallet_usdc_ata.clone(),
            1,
        ),
        SetupStage::ApproveTokenDelegate => (
            EARN_AUTODEPOSIT_WALLET_ATAS,
            state.wallet_usdc_ata.clone(),
            1,
        ),
    };
    Ok(SubscribeUpdate {
        filters: vec![filter.to_owned()],
        created_at: None,
        update_oneof: Some(UpdateOneof::Account(SubscribeUpdateAccount {
            account: Some(SubscribeUpdateAccountInfo {
                pubkey: Pubkey::from_str(&account_pubkey)?.to_bytes().to_vec(),
                lamports,
                owner: Vec::new(),
                executable: false,
                rent_epoch: 0,
                data: Vec::new(),
                write_version: 1,
                txn_signature: Some(
                    Signature::from_str(&transaction.signature)?
                        .as_ref()
                        .to_vec(),
                ),
            }),
            slot: transaction.slot,
            is_startup: false,
        })),
    })
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    let state: LocalState = serde_json::from_str(&fs::read_to_string(args.state)?)?;
    let store = OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url)).await?;
    let monitor = Mutex::new(PolicyMonitor::new(
        MonitorConfig {
            cluster: Cluster::Mainnet,
            commitment: Commitment::Finalized,
            ws_url: "local-targeted-account-emulator".to_owned(),
        },
        PostgresPolicyMatchSink::from_store(store.clone()),
    ));
    let chain = RpcEarnChainReader::new(args.rpc_url, store.clone());
    let consumer_name = "ask-2211-local-autodeposit";
    let claim_owner = "ask-2211-local-autodeposit-e2e";
    let persisted_targets = store.load_earn_subscription_targets("mainnet-beta").await?;
    let mut watch_set = if persisted_targets.is_empty() {
        initial_watch_set(&state)?
    } else {
        SubscriptionWatchSet::from_targets(Vec::new(), persisted_targets)?
    };
    let mut saw_policy = false;
    let mut saw_recurring_delegation = false;
    let mut saw_close = false;
    let mut observed_chain_status = None;

    for (line_number, line) in fs::read_to_string(args.transactions)?
        .lines()
        .enumerate()
        .filter(|(_, line)| !line.trim().is_empty())
    {
        let transaction: ChainTransaction = serde_json::from_str(line)
            .map_err(|error| anyhow::anyhow!("decode line {}: {error}", line_number + 1))?;
        let is_recurring_delegation =
            matches!(transaction.stage, SetupStage::CreateRecurringDelegation);
        let is_close = matches!(transaction.stage, SetupStage::CloseAutodeposit);
        saw_policy |= matches!(transaction.stage, SetupStage::CreatePolicy);
        saw_recurring_delegation |= is_recurring_delegation;
        saw_close |= is_close;
        let update = normalize_laserstream_update(emulated_update(&state, transaction.clone())?)?
            .ok_or_else(|| {
            anyhow::anyhow!("emulated LaserStream account update was ignored")
        })?;
        enqueue_normalized_earn_update(&store, consumer_name, &update, &watch_set).await?;
        if is_recurring_delegation {
            match process_next_autodeposit_reconciliation_request(
                &store,
                claim_owner,
                &chain,
                120,
                1,
            )
            .await?
            {
                AutodepositReconciliationProcessOutcome::AwaitingSetup {
                    requested_slot, ..
                } if requested_slot == update.slot => {}
                outcome => anyhow::bail!(
                    "same-slot token update did not put incomplete setup to sleep: {outcome:?}"
                ),
            }
            let policy_sibling = normalize_laserstream_update(emulated_update(
                &state,
                ChainTransaction {
                    stage: SetupStage::InitializeSubscriptionAuthority,
                    ..transaction
                },
            )?)?
            .ok_or_else(|| {
                anyhow::anyhow!("emulated same-slot policy account update was ignored")
            })?;
            enqueue_normalized_earn_update(&store, consumer_name, &policy_sibling, &watch_set)
                .await?;
        }
        if is_close {
            match process_next_autodeposit_reconciliation_request(
                &store,
                claim_owner,
                &chain,
                120,
                1,
            )
            .await?
            {
                AutodepositReconciliationProcessOutcome::Completed {
                    chain_status,
                    still_pending,
                    ..
                } if chain_status == "closed" && !still_pending => {
                    observed_chain_status = Some(chain_status);
                }
                outcome => anyhow::bail!(
                    "finalized Autodeposit close did not reconcile as closed: {outcome:?}"
                ),
            }
        }
        if is_recurring_delegation {
            let legacy_policy = PolicyMatchInput {
                signature: "legacy-policy-observation".to_owned(),
                slot: update.slot.saturating_sub(1),
                cluster: "mainnet-beta".to_owned(),
                source_commitment: "unknown".to_owned(),
                settings: state.settings_pda.clone(),
                authority: state.wallet_address.clone(),
                policy_seed: 999,
                policy_account: state.subscription_authority.clone(),
                vault_index: 1,
                vault_pubkey: state.vault_pubkey.clone(),
                delegated_signers: Vec::new(),
                threshold: 1,
                route_modes: vec!["same_mint_kamino".to_owned()],
                stable_mints: Vec::new(),
                kamino_markets: Vec::new(),
                kamino_liquidity_mints: Vec::new(),
                universe_preset: None,
                risk_profile: None,
                swap_lanes: json!([]),
            };
            store.record_policy_match(legacy_policy.clone()).await?;
            let repaired = store
                .record_policy_match(PolicyMatchInput {
                    source_commitment: "finalized".to_owned(),
                    slot: update.slot,
                    ..legacy_policy
                })
                .await?;
            if repaired.policy.source_commitment != "finalized" {
                anyhow::bail!("legacy unknown policy commitment was not repaired");
            }
            for _ in 0..300 {
                if args.pending_floor_ready.exists() {
                    break;
                }
                tokio::time::sleep(std::time::Duration::from_millis(100)).await;
            }
            if !args.pending_floor_ready.exists() {
                anyhow::bail!("web did not persist the floor on the pending target");
            }
        }
        let mut completed_jobs = 0;
        loop {
            match process_next_earn_reconciliation_job_with_policy_monitor(
                &store,
                consumer_name,
                claim_owner,
                &chain,
                Some(&monitor),
                120,
                1,
            )
            .await?
            {
                EarnReconciliationProcessOutcome::Completed { .. } => completed_jobs += 1,
                EarnReconciliationProcessOutcome::Idle => break,
                outcome => {
                    anyhow::bail!("targeted account reconciliation did not complete: {outcome:?}")
                }
            }
        }
        if completed_jobs == 0 {
            anyhow::bail!("targeted account notification created no durable reconciliation job");
        }

        if saw_policy || saw_close {
            let targets = store.load_earn_subscription_targets("mainnet-beta").await?;
            if targets.is_empty() {
                watch_set = initial_watch_set(&state)?;
            } else {
                watch_set = SubscriptionWatchSet::from_targets(Vec::new(), targets)?;
            }
        }
        if saw_policy && !saw_recurring_delegation {
            let target_id = store
                .load_autodeposit_reconciliation_target_id(
                    &state.settings_pda,
                    &state.vault_pubkey,
                    &state.policy_account,
                )
                .await?
                .context("partial setup policy target was not indexed")?;
            store
                .enqueue_autodeposit_reconciliation_request(target_id, update.slot)
                .await?;
            match process_next_autodeposit_reconciliation_request(
                &store,
                claim_owner,
                &chain,
                120,
                1,
            )
            .await?
            {
                AutodepositReconciliationProcessOutcome::AwaitingSetup { .. } => {}
                outcome => anyhow::bail!(
                    "partial Autodeposit setup was not retained as incomplete: {outcome:?}"
                ),
            }
        }
    }
    if !saw_close && !(saw_policy && saw_recurring_delegation) {
        anyhow::bail!("emulated stream missed policy or recurring-delegation setup");
    }
    if saw_close && (saw_policy || saw_recurring_delegation) {
        anyhow::bail!("close verification must use a separate finalized transaction stream");
    }

    let request = subscribe_request_json(&watch_set);
    fs::write(
        args.subscribe_request_output,
        serde_json::to_vec_pretty(&request)?,
    )?;
    let request_text = request.to_string();
    if saw_close {
        for (label, closed_account) in [
            ("policy account", &state.policy_account),
            ("subscription authority", &state.subscription_authority),
            ("recurring delegation", &state.recurring_delegation),
        ] {
            if request_text.contains(closed_account) {
                anyhow::bail!("refreshed subscription retained closed {label} {closed_account}");
            }
        }
    } else {
        for (label, expected_account) in [
            ("policy account", &state.policy_account),
            ("subscription authority", &state.subscription_authority),
            ("recurring delegation", &state.recurring_delegation),
            ("wallet USDC ATA", &state.wallet_usdc_ata),
        ] {
            if request_text.contains(expected_account) {
                continue;
            }
            let watched_policy_accounts = watch_set
                .earn_vaults
                .iter()
                .flat_map(|vault| vault.accounts.iter())
                .filter(|account| account.role == "policy")
                .map(|account| account.pubkey.as_str())
                .collect::<Vec<_>>();
            anyhow::bail!(
                "refreshed subscription omitted {label} {expected_account}; watched policies: {watched_policy_accounts:?}"
            );
        }
    }

    let expected_status = if saw_close { "closed" } else { "active" };
    loop {
        match process_next_autodeposit_reconciliation_request(&store, claim_owner, &chain, 120, 1)
            .await?
        {
            AutodepositReconciliationProcessOutcome::Idle => break,
            outcome @ AutodepositReconciliationProcessOutcome::AwaitingSetup { .. } => {
                anyhow::bail!("completed setup remained incomplete: {outcome:?}");
            }
            AutodepositReconciliationProcessOutcome::Completed {
                chain_status,
                still_pending,
                ..
            } => {
                if chain_status == expected_status && !still_pending {
                    observed_chain_status = Some(chain_status);
                }
            }
            outcome @ AutodepositReconciliationProcessOutcome::Deferred { .. } => {
                anyhow::bail!("Autodeposit snapshot reconciliation did not complete: {outcome:?}");
            }
        }
    }
    if observed_chain_status.as_deref() != Some(expected_status) {
        anyhow::bail!("Autodeposit target never reached {expected_status} chain state");
    }
    Ok(())
}
