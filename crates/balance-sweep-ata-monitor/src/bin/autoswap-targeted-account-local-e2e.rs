use std::{fs, path::PathBuf};

use balance_sweep_ata_monitor::{
    enqueue_normalized_earn_update, process_next_earn_reconciliation_job_with_policy_monitor,
    EarnReconciliationProcessOutcome, NormalizedEarnUpdate, RpcEarnChainReader,
    SubscriptionWatchSet, EARN_POLICY_ACCOUNTS, EARN_SMART_ACCOUNTS,
};
use clap::Parser;
use loyal_squads_policy_monitor::{
    Cluster, Commitment, MonitorConfig, PolicyMonitor, PostgresPolicyMatchSink,
};
use loyal_yield_store::{EarnSubscriptionTarget, OrchestratorConfig, OrchestratorStore};
use serde::Deserialize;
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
    account_kind: String,
    #[arg(long, default_value = "finalized")]
    policy_commitment: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LocalState {
    policies: Vec<String>,
    settings_pda: String,
    vault_pubkey: String,
    wallet_address: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ChainTransaction {
    signature: String,
    slot: u64,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    let state: LocalState = serde_json::from_str(&fs::read_to_string(args.state)?)?;
    let (filter, account, event_kind, policy_accounts) = match args.account_kind.as_str() {
        "smart-account" => (
            EARN_SMART_ACCOUNTS,
            state.settings_pda.clone(),
            "account",
            Vec::new(),
        ),
        "policy-deleted" => (
            EARN_POLICY_ACCOUNTS,
            state
                .policies
                .first()
                .cloned()
                .ok_or_else(|| anyhow::anyhow!("local state has no Autoswap policy"))?,
            "account_deleted",
            state.policies.clone(),
        ),
        other => anyhow::bail!("unsupported --account-kind {other}"),
    };
    let watch_set = SubscriptionWatchSet::from_targets(
        Vec::new(),
        vec![EarnSubscriptionTarget {
            environment: "mainnet-beta".to_owned(),
            settings: state.settings_pda,
            wallet: state.wallet_address,
            earn_max: false,
            vault_index: 1,
            vault_pubkey: Some(state.vault_pubkey),
            policy_accounts,
            markets: Vec::new(),
            autodeposit_accounts: Vec::new(),
            observation_start_slot: None,
        }],
    )?;
    let store = OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url)).await?;
    let policy_commitment = match args.policy_commitment.as_str() {
        "confirmed" => Commitment::Confirmed,
        "finalized" => Commitment::Finalized,
        other => anyhow::bail!("unsupported --policy-commitment {other}"),
    };
    let monitor = Mutex::new(PolicyMonitor::new(
        MonitorConfig {
            cluster: Cluster::Mainnet,
            commitment: policy_commitment,
            ws_url: "local-targeted-account-emulator".to_owned(),
        },
        PostgresPolicyMatchSink::from_store(store.clone()),
    ));
    let chain = RpcEarnChainReader::new(args.rpc_url, store.clone());
    let consumer_name = "ask-2168-local-targeted-account";
    let claim_owner = "ask-2168-local-targeted-account-e2e";
    for (line_number, line) in fs::read_to_string(args.transactions)?
        .lines()
        .enumerate()
        .filter(|(_, line)| !line.trim().is_empty())
    {
        let transaction: ChainTransaction = serde_json::from_str(line)
            .map_err(|error| anyhow::anyhow!("decode line {}: {error}", line_number + 1))?;
        let update = NormalizedEarnUpdate {
            event_key: None,
            filters: vec![filter.to_owned()],
            event_kind: event_kind.to_owned(),
            account_pubkey: Some(account.clone()),
            slot: transaction.slot,
            signature: Some(transaction.signature),
        };
        enqueue_normalized_earn_update(&store, consumer_name, &update, &watch_set).await?;
        let outcome = process_next_earn_reconciliation_job_with_policy_monitor(
            &store,
            consumer_name,
            claim_owner,
            &chain,
            Some(&monitor),
            120,
            15,
        )
        .await?;
        if !matches!(outcome, EarnReconciliationProcessOutcome::Completed { .. }) {
            anyhow::bail!("targeted account reconciliation did not complete: {outcome:?}");
        }
    }
    Ok(())
}
