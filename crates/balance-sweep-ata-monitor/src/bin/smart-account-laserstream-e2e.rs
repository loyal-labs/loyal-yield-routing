use std::{fs, path::PathBuf};

use anyhow::{Context, Result};
use balance_sweep_ata_monitor::{
    reconcile_normalized_earn_update, subscribe_request_json, FixtureEarnChainReader,
    NormalizedEarnUpdate, SubscriptionWatchSet,
};
use clap::Parser;
use loyal_yield_store::{OrchestratorConfig, OrchestratorStore};

#[derive(Debug, Parser)]
struct Args {
    #[arg(long)]
    postgres_url: String,
    #[arg(long)]
    stream_name: String,
    #[arg(
        long,
        conflicts_with = "environment",
        required_unless_present = "environment"
    )]
    watch_set: Option<PathBuf>,
    #[arg(
        long,
        conflicts_with = "watch_set",
        required_unless_present = "watch_set"
    )]
    environment: Option<String>,
    #[arg(long)]
    events: PathBuf,
    #[arg(long)]
    chain_fixtures: PathBuf,
    #[arg(long)]
    request_output: PathBuf,
    #[arg(long)]
    context_output: Option<PathBuf>,
}

#[derive(Debug, serde::Deserialize)]
struct FixtureEvent {
    event_key: String,
    kind: String,
    filters: Vec<String>,
    pubkey: Option<String>,
    slot: u64,
    signature: Option<String>,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    let store = OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url)).await?;
    let watch_set: SubscriptionWatchSet = if let Some(path) = args.watch_set {
        serde_json::from_str(&fs::read_to_string(path).context("read watch set")?)?
    } else {
        let environment = args.environment.context("environment is required")?;
        SubscriptionWatchSet::from_targets(
            Vec::new(),
            store.load_earn_subscription_targets(&environment).await?,
        )?
    };
    fs::write(
        &args.request_output,
        serde_json::to_vec_pretty(&subscribe_request_json(&watch_set))?,
    )?;
    if let Some(path) = args.context_output {
        let mut contexts = Vec::with_capacity(watch_set.earn_vaults.len());
        for vault in &watch_set.earn_vaults {
            let context = store
                .load_earn_reconciliation_context(&vault.settings, vault.vault_index, &vault.vault)
                .await?;
            contexts.push(serde_json::json!({
                "vault": vault.vault,
                "context": context,
            }));
        }
        fs::write(path, serde_json::to_vec_pretty(&contexts)?)?;
    }
    let chain = FixtureEarnChainReader::from_path(&args.chain_fixtures)?;

    for line in fs::read_to_string(&args.events)?
        .lines()
        .filter(|line| !line.trim().is_empty())
    {
        let event: FixtureEvent = serde_json::from_str(line).context("decode fixture event")?;
        let update = NormalizedEarnUpdate {
            event_key: Some(event.event_key.clone()),
            filters: event.filters,
            event_kind: if event.kind == "account_deleted" {
                "account_deleted"
            } else {
                "account"
            },
            account_pubkey: event.pubkey,
            slot: event.slot,
            signature: event.signature,
        };
        if watch_set
            .affected_vaults(update.account_pubkey.iter().map(String::as_str))
            .is_empty()
        {
            continue;
        }
        if std::env::var("SMART_ACCOUNT_E2E_FAIL_BEFORE_COMMIT_EVENT_KEY")
            .ok()
            .as_deref()
            == Some(event.event_key.as_str())
        {
            anyhow::bail!(
                "forced failure before direct Earn reconciliation commit for {}",
                event.event_key
            );
        }
        reconcile_normalized_earn_update(&store, &args.stream_name, &update, &watch_set, &chain)
            .await?;
    }
    Ok(())
}
