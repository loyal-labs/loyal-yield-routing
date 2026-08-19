use std::{fs, path::PathBuf};

use anyhow::{Context, Result};
use balance_sweep_ata_monitor::{
    earn_jobs_for_update, subscribe_request_json, NormalizedEarnUpdate, SubscriptionWatchSet,
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
    request_output: PathBuf,
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

    let mut max_slot = 0_u64;
    for line in fs::read_to_string(&args.events)?
        .lines()
        .filter(|line| !line.trim().is_empty())
    {
        let event: FixtureEvent = serde_json::from_str(line).context("decode fixture event")?;
        max_slot = max_slot.max(event.slot);
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
        let jobs = earn_jobs_for_update(&update, &watch_set);
        if jobs.is_empty() {
            continue;
        }
        if std::env::var("SMART_ACCOUNT_E2E_FAIL_BEFORE_COMMIT_EVENT_KEY")
            .ok()
            .as_deref()
            == Some(event.event_key.as_str())
        {
            anyhow::bail!(
                "forced failure before durable Earn receipt commit for {}",
                event.event_key
            );
        }
        store
            .record_earn_reconciliation_batch(&args.stream_name, event.slot, &jobs)
            .await?;
    }
    if max_slot == 0 {
        store
            .record_earn_reconciliation_batch(&args.stream_name, 0, &[])
            .await?;
    }
    Ok(())
}
