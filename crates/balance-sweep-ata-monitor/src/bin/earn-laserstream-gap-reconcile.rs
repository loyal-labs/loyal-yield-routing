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

    /// Audit only identities currently returned to the live monitor.
    #[arg(long)]
    live_targets_only: bool,

    /// Enqueue only candidates that the audit found missing. Omit for read-only audit mode.
    #[arg(long)]
    execute: bool,

    /// Save the complete audit report as JSON for operator review.
    #[arg(long, default_value = "earn-laserstream-gap-report.json")]
    report_file: PathBuf,
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

#[derive(Debug, Clone, Serialize, PartialEq, Eq, PartialOrd, Ord)]
#[serde(rename_all = "snake_case")]
enum CandidateCoverageStatus {
    Completed,
    Missing,
    Pending,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct CandidateJobReport {
    accounts: Vec<String>,
    filters: Vec<String>,
    signature: String,
    slot: u64,
    environment: String,
    wallet: String,
    settings: String,
    vault_index: u8,
    vault_pubkey: String,
    status: CandidateCoverageStatus,
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
    completed_jobs: usize,
    pending_jobs: usize,
    missing_jobs: usize,
    inserted_jobs: usize,
    coalesced_autodeposit_requests: usize,
    execution_requested: bool,
    candidates: Vec<CandidateJobReport>,
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
    let existing_coverage = store
        .load_earn_reconciliation_signature_coverage(&consumer_name, args.from_slot, to_slot)
        .await?;
    let coverage_by_key = existing_coverage
        .into_iter()
        .map(|coverage| {
            (
                (
                    coverage.signature,
                    coverage.settings,
                    coverage.vault_index,
                    coverage.vault_pubkey,
                ),
                (coverage.completed, coverage.pending),
            )
        })
        .collect::<BTreeMap<_, _>>();

    let mut candidates_by_key = BTreeMap::new();
    let mut missing_keys = BTreeSet::new();
    for planned_update in &planned {
        let signature = planned_update
            .update
            .signature
            .as_deref()
            .context("planned gap update is missing its signature")?;
        let account = planned_update
            .update
            .account_pubkey
            .as_deref()
            .context("planned gap update is missing its account")?;
        for vault in watch_set.affected_vaults(Some(account)) {
            let key = (
                signature.to_owned(),
                vault.settings.clone(),
                vault.vault_index,
                vault.vault.clone(),
            );
            let status = match coverage_by_key.get(&key) {
                Some((true, _)) => CandidateCoverageStatus::Completed,
                Some((false, true)) => CandidateCoverageStatus::Pending,
                _ => {
                    missing_keys.insert(key.clone());
                    CandidateCoverageStatus::Missing
                }
            };
            candidates_by_key
                .entry(key)
                .and_modify(|candidate: &mut CandidateJobReport| {
                    candidate.accounts.push(account.to_owned());
                    candidate.accounts.sort();
                    candidate.accounts.dedup();
                    candidate
                        .filters
                        .extend(planned_update.update.filters.clone());
                    candidate.filters.sort();
                    candidate.filters.dedup();
                })
                .or_insert_with(|| CandidateJobReport {
                    accounts: vec![account.to_owned()],
                    filters: planned_update.update.filters.clone(),
                    signature: signature.to_owned(),
                    slot: planned_update.update.slot,
                    environment: vault.environment.clone(),
                    wallet: vault.wallet.clone(),
                    settings: vault.settings.clone(),
                    vault_index: vault.vault_index,
                    vault_pubkey: vault.vault.clone(),
                    status,
                });
        }
    }
    let mut candidates = candidates_by_key.into_values().collect::<Vec<_>>();
    candidates.sort_by(|left, right| {
        (left.slot, &left.signature, &left.vault_pubkey).cmp(&(
            right.slot,
            &right.signature,
            &right.vault_pubkey,
        ))
    });
    let completed_jobs = candidates
        .iter()
        .filter(|candidate| candidate.status == CandidateCoverageStatus::Completed)
        .count();
    let pending_jobs = candidates
        .iter()
        .filter(|candidate| candidate.status == CandidateCoverageStatus::Pending)
        .count();
    let missing_jobs = candidates
        .iter()
        .filter(|candidate| candidate.status == CandidateCoverageStatus::Missing)
        .count();

    let mut report = ReconciliationReport {
        consumer_name: consumer_name.clone(),
        from_slot: args.from_slot,
        to_slot,
        selected_vaults: watch_set.earn_vaults.len(),
        accounts_scanned: earn_accounts_by_filter(&watch_set).len(),
        signatures_examined,
        successful_signatures_in_range,
        planned_updates: planned.len(),
        candidate_jobs: candidates.len(),
        first_planned_slot: planned.first().map(|planned| planned.update.slot),
        last_planned_slot: planned.last().map(|planned| planned.update.slot),
        completed_jobs,
        pending_jobs,
        missing_jobs,
        inserted_jobs: 0,
        coalesced_autodeposit_requests: 0,
        execution_requested: args.execute,
        candidates,
    };
    write_report(&args.report_file, &report)?;

    if args.execute {
        let mut remaining_missing_keys = missing_keys;
        let mut execution_plans = planned.iter().collect::<Vec<_>>();
        execution_plans.sort_by_key(|planned_update| {
            (
                is_policy_discovery_update(&planned_update.update),
                planned_update.update.slot,
                planned_update.update.account_pubkey.clone(),
            )
        });
        for planned_update in execution_plans {
            let signature = planned_update
                .update
                .signature
                .as_deref()
                .context("planned gap update is missing its signature")?;
            let mut missing_watch_set = watch_set.clone();
            missing_watch_set.earn_vaults.retain(|vault| {
                remaining_missing_keys.contains(&(
                    signature.to_owned(),
                    vault.settings.clone(),
                    vault.vault_index,
                    vault.vault.clone(),
                ))
            });
            if missing_watch_set.earn_vaults.is_empty() {
                continue;
            }
            let enqueued_keys = missing_watch_set
                .earn_vaults
                .iter()
                .map(|vault| {
                    (
                        signature.to_owned(),
                        vault.settings.clone(),
                        vault.vault_index,
                        vault.vault.clone(),
                    )
                })
                .collect::<Vec<_>>();
            let outcome = enqueue_normalized_earn_update(
                &store,
                &consumer_name,
                &planned_update.update,
                &missing_watch_set,
            )
            .await?;
            report.inserted_jobs = report.inserted_jobs.saturating_add(outcome.inserted_jobs);
            report.coalesced_autodeposit_requests = report
                .coalesced_autodeposit_requests
                .saturating_add(outcome.coalesced_autodeposit_requests);
            for key in enqueued_keys {
                remaining_missing_keys.remove(&key);
            }
        }
        write_report(&args.report_file, &report)?;
    }
    println!("{}", serde_json::to_string_pretty(&report)?);
    Ok(())
}

fn write_report(path: &Path, report: &ReconciliationReport) -> Result<()> {
    let mut json = serde_json::to_vec_pretty(report)?;
    json.push(b'\n');
    fs::write(path, json).with_context(|| format!("write audit report {}", path.display()))
}

fn is_policy_discovery_update(update: &NormalizedEarnUpdate) -> bool {
    update.filters.iter().any(|filter| {
        matches!(
            filter.as_str(),
            "earn_smart_accounts" | "earn_policy_accounts" | "earn_wallets"
        )
    })
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
    if !args.live_targets_only {
        targets.extend(
            store
                .load_earn_historical_reconciliation_targets(
                    &args.environment,
                    args.wallet.as_deref(),
                )
                .await?,
        );
    }
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
