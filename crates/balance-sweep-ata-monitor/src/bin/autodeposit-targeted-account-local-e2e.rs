use std::{fs, path::PathBuf, str::FromStr};

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
use loyal_yield_store::{EarnSubscriptionTarget, OrchestratorConfig, OrchestratorStore};
use serde::Deserialize;
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

#[derive(Debug, Deserialize)]
#[serde(rename_all = "snake_case")]
enum SetupStage {
    ApproveTokenDelegate,
    CreatePolicy,
    CreateRecurringDelegation,
    InitializeSubscriptionAuthority,
}

#[derive(Debug, Deserialize)]
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
    let (filter, account_pubkey) = match transaction.stage {
        SetupStage::InitializeSubscriptionAuthority => (EARN_WALLETS, state.wallet_address.clone()),
        SetupStage::CreatePolicy => (EARN_SMART_ACCOUNTS, state.settings_pda.clone()),
        SetupStage::CreateRecurringDelegation => (EARN_WALLETS, state.wallet_address.clone()),
        SetupStage::ApproveTokenDelegate => {
            (EARN_AUTODEPOSIT_WALLET_ATAS, state.wallet_usdc_ata.clone())
        }
    };
    Ok(SubscribeUpdate {
        filters: vec![filter.to_owned()],
        created_at: None,
        update_oneof: Some(UpdateOneof::Account(SubscribeUpdateAccount {
            account: Some(SubscribeUpdateAccountInfo {
                pubkey: Pubkey::from_str(&account_pubkey)?.to_bytes().to_vec(),
                lamports: 1,
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
    let mut watch_set = initial_watch_set(&state)?;
    let mut saw_policy = false;
    let mut saw_recurring_delegation = false;

    for (line_number, line) in fs::read_to_string(args.transactions)?
        .lines()
        .enumerate()
        .filter(|(_, line)| !line.trim().is_empty())
    {
        let transaction: ChainTransaction = serde_json::from_str(line)
            .map_err(|error| anyhow::anyhow!("decode line {}: {error}", line_number + 1))?;
        saw_policy |= matches!(transaction.stage, SetupStage::CreatePolicy);
        saw_recurring_delegation |=
            matches!(transaction.stage, SetupStage::CreateRecurringDelegation);
        let update = normalize_laserstream_update(emulated_update(&state, transaction)?)?
            .ok_or_else(|| anyhow::anyhow!("emulated LaserStream account update was ignored"))?;
        enqueue_normalized_earn_update(&store, consumer_name, &update, &watch_set).await?;
        let outcome = process_next_earn_reconciliation_job_with_policy_monitor(
            &store,
            consumer_name,
            claim_owner,
            &chain,
            Some(&monitor),
            120,
            1,
        )
        .await?;
        if !matches!(outcome, EarnReconciliationProcessOutcome::Completed { .. }) {
            anyhow::bail!("targeted account reconciliation did not complete: {outcome:?}");
        }

        if saw_policy {
            let targets = store.load_earn_subscription_targets("mainnet-beta").await?;
            if !targets.is_empty() {
                watch_set = SubscriptionWatchSet::from_targets(Vec::new(), targets)?;
            }
        }
    }
    if !(saw_policy && saw_recurring_delegation) {
        anyhow::bail!("emulated stream missed policy or recurring-delegation setup");
    }

    let request = subscribe_request_json(&watch_set);
    fs::write(
        args.subscribe_request_output,
        serde_json::to_vec_pretty(&request)?,
    )?;
    let request_text = request.to_string();
    for (label, expected_account) in [
        ("policy account", &state.policy_account),
        ("subscription authority", &state.subscription_authority),
        ("recurring delegation", &state.recurring_delegation),
        ("wallet USDC ATA", &state.wallet_usdc_ata),
    ] {
        if !request_text.contains(expected_account) {
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

    let mut completed = None;
    loop {
        match process_next_autodeposit_reconciliation_request(&store, claim_owner, &chain, 120, 1)
            .await?
        {
            AutodepositReconciliationProcessOutcome::Idle => break,
            AutodepositReconciliationProcessOutcome::Completed {
                chain_status,
                still_pending,
                ..
            } => {
                if chain_status == "active" && !still_pending {
                    completed = Some(chain_status);
                }
            }
            outcome @ AutodepositReconciliationProcessOutcome::Deferred { .. } => {
                anyhow::bail!("Autodeposit snapshot reconciliation did not complete: {outcome:?}");
            }
        }
    }
    if completed.as_deref() != Some("active") {
        anyhow::bail!("Autodeposit target never reached active chain state");
    }
    Ok(())
}
