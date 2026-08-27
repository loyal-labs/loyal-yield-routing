use std::{
    collections::{BTreeMap, BTreeSet},
    fs,
    path::{Path, PathBuf},
    str::FromStr,
    sync::Arc,
};

use anyhow::{bail, Context, Result};
use balance_sweep_ata_monitor::{
    enqueue_normalized_earn_update, NormalizedEarnUpdate, SubscriptionWatchSet,
};
use clap::Parser;
use futures_util::{stream, StreamExt, TryStreamExt};
use loyal_yield_store::{OrchestratorConfig, OrchestratorStore};
use serde::{Deserialize, Serialize};
use solana_client::{
    rpc_client::{GetConfirmedSignaturesForAddress2Config, RpcClient},
    rpc_response::RpcConfirmedTransactionStatusWithSignature,
};
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey, signature::Signature};

const HISTORY_PAGE_SIZE: usize = 1_000;

#[derive(Debug, Parser)]
#[command(
    about = "Backfill durable Earn reconciliation jobs from finalized Solana account history"
)]
struct Args {
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,

    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: Option<String>,

    #[arg(long, default_value = "mainnet")]
    environment: String,

    #[arg(long)]
    consumer_name: Option<String>,

    #[arg(long)]
    from_slot: u64,

    #[arg(long)]
    to_slot: Option<u64>,

    #[arg(long)]
    wallet: Option<String>,

    #[arg(long, default_value_t = 20_000)]
    max_signatures_per_account: usize,

    #[arg(long, default_value_t = 8)]
    concurrency: usize,

    #[arg(long, conflicts_with = "rpc_url")]
    history_fixture: Option<PathBuf>,

    #[arg(long)]
    watch_set: Option<PathBuf>,

    #[arg(long)]
    dry_run: bool,
}

#[derive(Debug, Clone, Deserialize)]
struct HistoryFixture {
    finalized_slot: u64,
    accounts: BTreeMap<String, Vec<FixtureSignature>>,
}

#[derive(Debug, Clone, Deserialize)]
struct FixtureSignature {
    signature: String,
    slot: u64,
    #[serde(default)]
    failed: bool,
}

#[derive(Debug, Clone)]
struct HistoryRecord {
    signature: String,
    slot: u64,
    failed: bool,
}

#[derive(Debug, Clone)]
struct AccountScan {
    account: String,
    filters: BTreeSet<String>,
    records: Vec<HistoryRecord>,
    signatures_examined: usize,
}

#[derive(Debug, Clone)]
struct PlannedUpdate {
    update: NormalizedEarnUpdate,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ReconciliationReport {
    consumer_name: String,
    from_slot: u64,
    to_slot: u64,
    selected_vaults: usize,
    accounts_scanned: usize,
    signatures_examined: usize,
    successful_signatures_in_range: usize,
    planned_updates: usize,
    candidate_jobs: usize,
    first_planned_slot: Option<u64>,
    last_planned_slot: Option<u64>,
    inserted_jobs: usize,
    existing_jobs: usize,
    coalesced_autodeposit_requests: usize,
    dry_run: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    if args.from_slot == 0 {
        bail!("--from-slot must be greater than zero");
    }
    if args.max_signatures_per_account == 0 {
        bail!("--max-signatures-per-account must be greater than zero");
    }
    if args.concurrency == 0 {
        bail!("--concurrency must be greater than zero");
    }

    let store =
        OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url.clone())).await?;
    let mut watch_set = load_watch_set(&args, &store).await?;
    if let Some(wallet) = args.wallet.as_deref() {
        Pubkey::from_str(wallet).context("--wallet must be a Solana pubkey")?;
        watch_set.earn_vaults.retain(|vault| vault.wallet == wallet);
    }
    if watch_set.earn_vaults.is_empty() {
        bail!("no Earn vaults matched the selected environment and wallet");
    }

    let accounts = earn_accounts_by_filter(&watch_set);
    if accounts.is_empty() {
        bail!("the selected Earn watch set contains no account subscriptions");
    }

    let fixture = args
        .history_fixture
        .as_deref()
        .map(load_history_fixture)
        .transpose()?;
    let rpc = args.rpc_url.as_ref().map(|url| {
        Arc::new(RpcClient::new_with_commitment(
            url.clone(),
            CommitmentConfig::finalized(),
        ))
    });
    if fixture.is_none() && rpc.is_none() {
        bail!("--rpc-url or --history-fixture is required");
    }

    let to_slot = match (args.to_slot, fixture.as_ref(), rpc.as_ref()) {
        (Some(slot), _, _) => slot,
        (None, Some(fixture), _) => fixture.finalized_slot,
        (None, None, Some(rpc)) => rpc
            .get_slot_with_commitment(CommitmentConfig::finalized())
            .context("read finalized Solana slot")?,
        (None, None, None) => unreachable!("history source was validated"),
    };
    if to_slot < args.from_slot {
        bail!("--to-slot must be greater than or equal to --from-slot");
    }
    if let Some(fixture) = fixture.as_ref() {
        if to_slot > fixture.finalized_slot {
            bail!(
                "requested end slot {to_slot} exceeds fixture finalized slot {}",
                fixture.finalized_slot
            );
        }
    }

    let scans = if let Some(fixture) = fixture.as_ref() {
        scan_fixture_accounts(
            accounts,
            fixture,
            args.from_slot,
            to_slot,
            args.max_signatures_per_account,
        )?
    } else {
        scan_live_accounts(
            accounts,
            rpc.context("live RPC client is missing")?,
            args.from_slot,
            to_slot,
            args.max_signatures_per_account,
            args.concurrency,
        )
        .await?
    };
    let signatures_examined = scans.iter().map(|scan| scan.signatures_examined).sum();
    let successful_signatures_in_range = scans
        .iter()
        .map(|scan| scan.records.iter().filter(|record| !record.failed).count())
        .sum();
    let planned = plan_updates(scans, args.from_slot, to_slot);

    let consumer_name = args
        .consumer_name
        .unwrap_or_else(|| format!("earn-smart-account:{}", args.environment));
    let mut inserted_jobs = 0_usize;
    let mut coalesced_autodeposit_requests = 0_usize;
    if !args.dry_run {
        for planned_update in &planned {
            let outcome = enqueue_normalized_earn_update(
                &store,
                &consumer_name,
                &planned_update.update,
                &watch_set,
            )
            .await?;
            inserted_jobs = inserted_jobs.saturating_add(outcome.inserted_jobs);
            coalesced_autodeposit_requests = coalesced_autodeposit_requests
                .saturating_add(outcome.coalesced_autodeposit_requests);
        }
    }
    let affected_jobs = planned
        .iter()
        .map(|planned_update| {
            watch_set
                .affected_vaults(
                    planned_update
                        .update
                        .account_pubkey
                        .iter()
                        .map(String::as_str),
                )
                .len()
        })
        .sum::<usize>();

    let report = ReconciliationReport {
        consumer_name,
        from_slot: args.from_slot,
        to_slot,
        selected_vaults: watch_set.earn_vaults.len(),
        accounts_scanned: earn_accounts_by_filter(&watch_set).len(),
        signatures_examined,
        successful_signatures_in_range,
        planned_updates: planned.len(),
        candidate_jobs: affected_jobs,
        first_planned_slot: planned.first().map(|planned| planned.update.slot),
        last_planned_slot: planned.last().map(|planned| planned.update.slot),
        inserted_jobs,
        existing_jobs: if args.dry_run {
            0
        } else {
            affected_jobs.saturating_sub(inserted_jobs)
        },
        coalesced_autodeposit_requests,
        dry_run: args.dry_run,
    };
    println!("{}", serde_json::to_string_pretty(&report)?);
    Ok(())
}

async fn load_watch_set(args: &Args, store: &OrchestratorStore) -> Result<SubscriptionWatchSet> {
    if let Some(path) = args.watch_set.as_deref() {
        return serde_json::from_slice(
            &fs::read(path).with_context(|| format!("read watch set {}", path.display()))?,
        )
        .context("decode watch set");
    }
    let mut targets = store
        .load_earn_subscription_targets(&args.environment)
        .await?;
    targets.extend(
        store
            .load_earn_historical_reconciliation_targets(&args.environment, args.wallet.as_deref())
            .await?,
    );
    SubscriptionWatchSet::from_targets(Vec::new(), targets)
}

fn load_history_fixture(path: &Path) -> Result<HistoryFixture> {
    serde_json::from_slice(
        &fs::read(path).with_context(|| format!("read history fixture {}", path.display()))?,
    )
    .context("decode history fixture")
}

fn earn_accounts_by_filter(watch_set: &SubscriptionWatchSet) -> BTreeMap<String, BTreeSet<String>> {
    let mut accounts = BTreeMap::<String, BTreeSet<String>>::new();
    for (filter, pubkeys) in watch_set.account_channels() {
        if !filter.starts_with("earn_") {
            continue;
        }
        for pubkey in pubkeys {
            accounts
                .entry(pubkey)
                .or_default()
                .insert(filter.to_owned());
        }
    }
    accounts
}

fn scan_fixture_accounts(
    accounts: BTreeMap<String, BTreeSet<String>>,
    fixture: &HistoryFixture,
    from_slot: u64,
    to_slot: u64,
    max_signatures_per_account: usize,
) -> Result<Vec<AccountScan>> {
    accounts
        .into_iter()
        .map(|(account, filters)| {
            let fixture_records = fixture.accounts.get(&account).cloned().unwrap_or_default();
            let signatures_examined = fixture_records
                .iter()
                .filter(|record| record.slot >= from_slot)
                .count();
            if signatures_examined > max_signatures_per_account {
                bail!(
                    "account {account} exceeded --max-signatures-per-account ({max_signatures_per_account})"
                );
            }
            let records = fixture_records
                .into_iter()
                .filter(|record| (from_slot..=to_slot).contains(&record.slot))
                .map(|record| HistoryRecord {
                    signature: record.signature,
                    slot: record.slot,
                    failed: record.failed,
                })
                .collect();
            Ok(AccountScan {
                account,
                filters,
                records,
                signatures_examined,
            })
        })
        .collect()
}

async fn scan_live_accounts(
    accounts: BTreeMap<String, BTreeSet<String>>,
    rpc: Arc<RpcClient>,
    from_slot: u64,
    to_slot: u64,
    max_signatures_per_account: usize,
    concurrency: usize,
) -> Result<Vec<AccountScan>> {
    stream::iter(accounts)
        .map(|(account, filters)| {
            let rpc = Arc::clone(&rpc);
            async move {
                tokio::task::spawn_blocking(move || {
                    scan_live_account(
                        rpc.as_ref(),
                        account,
                        filters,
                        from_slot,
                        to_slot,
                        max_signatures_per_account,
                    )
                })
                .await
                .context("Solana account-history task panicked")?
            }
        })
        .buffer_unordered(concurrency)
        .try_collect()
        .await
}

fn scan_live_account(
    rpc: &RpcClient,
    account: String,
    filters: BTreeSet<String>,
    from_slot: u64,
    to_slot: u64,
    max_signatures_per_account: usize,
) -> Result<AccountScan> {
    let pubkey =
        Pubkey::from_str(&account).with_context(|| format!("invalid watched account {account}"))?;
    let mut before = None;
    let mut records = Vec::new();
    let mut signatures_examined = 0_usize;
    loop {
        let page = rpc
            .get_signatures_for_address_with_config(
                &pubkey,
                GetConfirmedSignaturesForAddress2Config {
                    before,
                    until: None,
                    limit: Some(HISTORY_PAGE_SIZE),
                    commitment: Some(CommitmentConfig::finalized()),
                },
            )
            .with_context(|| format!("read finalized signature history for {account}"))?;
        if page.is_empty() {
            break;
        }
        let mut reached_start = false;
        for status in &page {
            if status.slot < from_slot {
                reached_start = true;
                break;
            }
            signatures_examined = signatures_examined.saturating_add(1);
            if signatures_examined > max_signatures_per_account {
                bail!(
                    "account {account} exceeded --max-signatures-per-account ({max_signatures_per_account}) before reaching slot {from_slot}"
                );
            }
            if status.slot <= to_slot {
                records.push(history_record(status));
            }
        }
        if reached_start || page.len() < HISTORY_PAGE_SIZE {
            break;
        }
        before = Some(Signature::from_str(
            &page
                .last()
                .context("Solana history page unexpectedly became empty")?
                .signature,
        )?);
    }
    Ok(AccountScan {
        account,
        filters,
        records,
        signatures_examined,
    })
}

fn history_record(status: &RpcConfirmedTransactionStatusWithSignature) -> HistoryRecord {
    HistoryRecord {
        signature: status.signature.clone(),
        slot: status.slot,
        failed: status.err.is_some(),
    }
}

fn plan_updates(scans: Vec<AccountScan>, from_slot: u64, to_slot: u64) -> Vec<PlannedUpdate> {
    let mut planned = BTreeMap::<(u64, String, String), BTreeSet<String>>::new();
    for scan in scans {
        for record in scan.records {
            if record.failed || !(from_slot..=to_slot).contains(&record.slot) {
                continue;
            }
            planned
                .entry((record.slot, record.signature, scan.account.clone()))
                .or_default()
                .extend(scan.filters.iter().cloned());
        }
    }
    planned
        .into_iter()
        .map(|((slot, signature, account), filters)| PlannedUpdate {
            update: NormalizedEarnUpdate {
                event_key: Some(format!("earn-rpc-gap:{slot}:{signature}:{account}")),
                filters: filters.into_iter().collect(),
                event_kind: "account".to_owned(),
                account_pubkey: Some(account),
                slot,
                signature: Some(signature),
            },
        })
        .collect()
}
