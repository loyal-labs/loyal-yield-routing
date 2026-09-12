use std::{
    collections::BTreeSet,
    fs,
    path::{Path, PathBuf},
    str::FromStr,
    sync::Arc,
};

use anyhow::{bail, Context, Result};
use balance_sweep_ata_monitor::{
    read_confirmed_squads_policy_transaction, EarnPolicyTransaction, EarnPolicyTransactionRead,
};
use clap::Parser;
use futures_util::{future::BoxFuture, stream, StreamExt, TryStreamExt};
use loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID;
use loyal_squads_policy_monitor::{
    BalanceSweepExecutionEvent, Cluster, Commitment, MonitorConfig, MonitorError, PolicyMatchSink,
    PolicyMonitor, PolicyMonitorEvent, PostgresPolicyMatchSink,
    EARN_MAX_POLICY_PROJECTION_CONSUMER,
};
use loyal_yield_store::{EarnMaxPolicySetProjectionInput, OrchestratorConfig, OrchestratorStore};
use serde::Serialize;
use solana_client::{
    rpc_client::{GetConfirmedSignaturesForAddress2Config, RpcClient},
    rpc_response::RpcConfirmedTransactionStatusWithSignature,
};
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey, signature::Signature};
use tokio::sync::Mutex;

const HISTORY_PAGE_SIZE: usize = 1_000;

#[derive(Debug, Parser)]
#[command(about = "Audit and repair missed Earn policy projections from finalized Squads history")]
struct Args {
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,

    #[arg(long, env = "SOLANA_RPC_URL")]
    rpc_url: String,

    #[arg(long, env = "EARN_MAX_DELEGATE")]
    earn_max_delegate: String,

    #[arg(long, value_enum, default_value_t = Cluster::Mainnet)]
    environment: Cluster,

    #[arg(long)]
    from_slot: u64,

    #[arg(long)]
    to_slot: Option<u64>,

    /// Squads signature after the range, used to avoid paging from current history.
    #[arg(long)]
    before_signature: Option<String>,

    #[arg(long, default_value_t = 100_000)]
    max_signatures: usize,

    #[arg(long, default_value_t = 8)]
    concurrency: usize,

    /// Apply the policy events found by the audit. Omit for read-only mode.
    #[arg(long)]
    execute: bool,

    /// Advance the live policy cursor after every audited transaction is replayed.
    #[arg(long, requires = "execute", requires = "expected_cursor")]
    advance_cursor: bool,

    /// Current cursor value required before an execution that advances it.
    #[arg(long)]
    expected_cursor: Option<u64>,

    #[arg(long, default_value = "earn-policy-projection-gap-report.json")]
    report_file: PathBuf,
}

#[derive(Debug, Clone)]
struct HistoryRecord {
    signature: String,
    slot: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
enum CoverageStatus {
    Completed,
    Missing,
}

fn policy_coverage(
    projected_state: Option<(u64, bool)>,
    event_slot: u64,
    is_removal: bool,
) -> CoverageStatus {
    if projected_state.is_some_and(|(projected_slot, _)| projected_slot >= event_slot)
        || (is_removal && projected_state.is_none_or(|(_, active)| !active))
    {
        CoverageStatus::Completed
    } else {
        CoverageStatus::Missing
    }
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct PolicyFinding {
    kind: String,
    signature: String,
    slot: u64,
    settings: String,
    policy_account: Option<String>,
    vault_index: u8,
    coverage: CoverageStatus,
}

fn repair_signatures(findings: &[PolicyFinding]) -> BTreeSet<String> {
    let missing_settings = findings
        .iter()
        .filter(|finding| matches!(finding.coverage, CoverageStatus::Missing))
        .map(|finding| finding.settings.as_str())
        .collect::<BTreeSet<_>>();
    findings
        .iter()
        .filter(|finding| missing_settings.contains(finding.settings.as_str()))
        .map(|finding| finding.signature.clone())
        .collect()
}

struct PolicyEventInput {
    kind: &'static str,
    cluster: String,
    signature: String,
    slot: u64,
    settings: String,
    policy_account: String,
    vault_index: u8,
}

#[derive(Debug, Default)]
struct AuditState {
    findings: Vec<PolicyFinding>,
}

#[derive(Clone)]
struct AuditPolicySink {
    store: OrchestratorStore,
    state: Arc<Mutex<AuditState>>,
}

impl AuditPolicySink {
    async fn record_policy(&self, input: PolicyEventInput) -> Result<(), MonitorError> {
        let projected_state = self
            .store
            .route_policy_projection_state(&input.cluster, &input.policy_account)
            .await?;
        let is_removal = input.kind == "policy_removal";
        let coverage = policy_coverage(projected_state, input.slot, is_removal);
        let mut state = self.state.lock().await;
        state.findings.push(PolicyFinding {
            kind: input.kind.to_owned(),
            signature: input.signature,
            slot: input.slot,
            settings: input.settings,
            policy_account: Some(input.policy_account),
            vault_index: input.vault_index,
            coverage,
        });
        Ok(())
    }
}

impl PolicyMatchSink for AuditPolicySink {
    fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async move {
            match event {
                PolicyMonitorEvent::YieldRoute(event) => {
                    self.record_policy(PolicyEventInput {
                        kind: "yield_route",
                        cluster: event.cluster.to_string(),
                        signature: event.signature,
                        slot: event.slot,
                        settings: event.settings,
                        policy_account: event.policy_account,
                        vault_index: event.vault_index,
                    })
                    .await?;
                }
                PolicyMonitorEvent::YieldSetup(event) => {
                    self.record_policy(PolicyEventInput {
                        kind: "yield_setup",
                        cluster: event.cluster.to_string(),
                        signature: event.signature,
                        slot: event.slot,
                        settings: event.settings,
                        policy_account: event.policy_account,
                        vault_index: event.vault_index,
                    })
                    .await?;
                }
                PolicyMonitorEvent::PolicyRemoval(event) => {
                    self.record_policy(PolicyEventInput {
                        kind: "policy_removal",
                        cluster: event.cluster.to_string(),
                        signature: event.signature,
                        slot: event.slot,
                        settings: event.settings,
                        policy_account: event.policy_account,
                        vault_index: 0,
                    })
                    .await?;
                }
                PolicyMonitorEvent::BalanceSweep(_)
                | PolicyMonitorEvent::CrossMintSwapPolicyManifest(_) => {}
            }
            Ok(())
        })
    }

    fn emit_execution(
        &mut self,
        _event: BalanceSweepExecutionEvent,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Ok(()) })
    }

    fn project_earn_max_policy_set(
        &mut self,
        input: EarnMaxPolicySetProjectionInput,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async move {
            let projected_slot = self
                .store
                .earn_max_policy_set_projection_slot(&input.settings, input.vault_index)
                .await?;
            let coverage =
                if projected_slot.is_some_and(|projected| projected >= input.observed_slot) {
                    CoverageStatus::Completed
                } else {
                    CoverageStatus::Missing
                };
            let mut state = self.state.lock().await;
            state.findings.push(PolicyFinding {
                kind: "earn_max_policy_set".to_owned(),
                signature: input.observed_signature,
                slot: input.observed_slot,
                settings: input.settings,
                policy_account: None,
                vault_index: input.vault_index,
                coverage,
            });
            Ok(())
        })
    }

    fn earn_max_policy_seed_base(
        &mut self,
        settings: Pubkey,
    ) -> BoxFuture<'_, Result<Option<u64>, MonitorError>> {
        Box::pin(async move {
            Ok(self
                .store
                .load_earn_max_policy_seed_base(&settings.to_string(), 1)
                .await?)
        })
    }
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct ReconciliationReport {
    consumer_name: &'static str,
    from_slot: u64,
    to_slot: u64,
    signatures_examined: usize,
    successful_transactions_in_range: usize,
    relevant_transactions: usize,
    policy_findings: usize,
    missing_findings: usize,
    execution_requested: bool,
    replayed_transactions: usize,
    cursor_advanced_to: Option<u64>,
    findings: Vec<PolicyFinding>,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args = Args::parse();
    if args.from_slot == 0 {
        bail!("--from-slot must be greater than zero");
    }
    if args.max_signatures == 0 {
        bail!("--max-signatures must be greater than zero");
    }
    if args.concurrency == 0 {
        bail!("--concurrency must be greater than zero");
    }

    let delegate = Pubkey::from_str(&args.earn_max_delegate)
        .context("EARN_MAX_DELEGATE must be a Solana pubkey")?;
    let store =
        OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url.clone())).await?;
    let rpc = Arc::new(RpcClient::new_with_commitment(
        args.rpc_url.clone(),
        CommitmentConfig::finalized(),
    ));
    let to_slot = match args.to_slot {
        Some(slot) => slot,
        None => rpc
            .get_slot_with_commitment(CommitmentConfig::finalized())
            .context("read finalized Solana slot")?,
    };
    if to_slot < args.from_slot {
        bail!("--to-slot must be greater than or equal to --from-slot");
    }

    if args.advance_cursor {
        require_projection_cursor(
            &store,
            args.expected_cursor
                .context("--expected-cursor is required")?,
        )
        .await?;
    }

    let before_signature = args
        .before_signature
        .as_deref()
        .map(Signature::from_str)
        .transpose()
        .context("--before-signature must be a Solana signature")?;
    let (history, signatures_examined) = scan_program_history(
        Arc::clone(&rpc),
        args.from_slot,
        to_slot,
        before_signature,
        args.max_signatures,
    )
    .await?;
    let transactions = load_transactions(Arc::clone(&rpc), history, args.concurrency).await?;
    let successful_transactions_in_range = transactions.len();

    let state = Arc::new(Mutex::new(AuditState::default()));
    let sink = AuditPolicySink {
        store: store.clone(),
        state: Arc::clone(&state),
    };
    let mut audit_monitor = PolicyMonitor::new(
        MonitorConfig {
            cluster: args.environment,
            commitment: Commitment::Finalized,
            ws_url: String::new(),
        },
        sink,
    )
    .with_earn_max_projection(args.rpc_url.clone(), delegate);

    for transaction in &transactions {
        audit_monitor
            .process_policy_instructions(
                &transaction.signature,
                transaction.slot,
                transaction.instructions.clone(),
            )
            .await?;
    }

    let mut audit = state.lock().await;
    audit.findings.sort_by(|left, right| {
        (left.slot, &left.signature, &left.kind).cmp(&(right.slot, &right.signature, &right.kind))
    });
    let findings = audit.findings.clone();
    drop(audit);
    let relevant_signatures = repair_signatures(&findings);
    let missing_findings = findings
        .iter()
        .filter(|finding| matches!(finding.coverage, CoverageStatus::Missing))
        .count();
    let mut report = ReconciliationReport {
        consumer_name: EARN_MAX_POLICY_PROJECTION_CONSUMER,
        from_slot: args.from_slot,
        to_slot,
        signatures_examined,
        successful_transactions_in_range,
        relevant_transactions: relevant_signatures.len(),
        policy_findings: findings.len(),
        missing_findings,
        execution_requested: args.execute,
        replayed_transactions: 0,
        cursor_advanced_to: None,
        findings,
    };
    write_report(&args.report_file, &report)?;

    if args.execute {
        if args.advance_cursor {
            require_projection_cursor(
                &store,
                args.expected_cursor
                    .context("--expected-cursor is required")?,
            )
            .await?;
        }
        let mut execution_monitor = PolicyMonitor::new(
            MonitorConfig {
                cluster: args.environment,
                commitment: Commitment::Finalized,
                ws_url: String::new(),
            },
            PostgresPolicyMatchSink::from_store(store.clone()),
        )
        .with_earn_max_projection(args.rpc_url, delegate);
        for transaction in transactions
            .iter()
            .filter(|transaction| relevant_signatures.contains(&transaction.signature))
        {
            execution_monitor
                .process_policy_instructions(
                    &transaction.signature,
                    transaction.slot,
                    transaction.instructions.clone(),
                )
                .await?;
            report.replayed_transactions += 1;
        }
        if args.advance_cursor {
            store
                .advance_projection_offset(
                    EARN_MAX_POLICY_PROJECTION_CONSUMER,
                    i64::try_from(to_slot).context("--to-slot exceeds BIGINT")?,
                )
                .await?;
            report.cursor_advanced_to = Some(to_slot);
        }
        write_report(&args.report_file, &report)?;
    }

    println!("{}", serde_json::to_string_pretty(&report)?);
    Ok(())
}

async fn require_projection_cursor(store: &OrchestratorStore, expected: u64) -> Result<()> {
    let current = u64::try_from(
        store
            .projection_offset(EARN_MAX_POLICY_PROJECTION_CONSUMER)
            .await?,
    )
    .context("policy projection cursor is negative")?;
    if current != expected {
        bail!("policy projection cursor changed: expected {expected}, found {current}");
    }
    Ok(())
}

async fn scan_program_history(
    rpc: Arc<RpcClient>,
    from_slot: u64,
    to_slot: u64,
    before_signature: Option<Signature>,
    max_signatures: usize,
) -> Result<(Vec<HistoryRecord>, usize)> {
    tokio::task::spawn_blocking(move || {
        let mut before = before_signature;
        let mut records = Vec::new();
        let mut signatures_examined = 0_usize;
        loop {
            let page = rpc
                .get_signatures_for_address_with_config(
                    &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
                    GetConfirmedSignaturesForAddress2Config {
                        before,
                        until: None,
                        limit: Some(HISTORY_PAGE_SIZE),
                        commitment: Some(CommitmentConfig::finalized()),
                    },
                )
                .context("read finalized Squads program history")?;
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
                if signatures_examined > max_signatures {
                    bail!(
                        "Squads program history exceeded --max-signatures ({max_signatures}) before reaching slot {from_slot}"
                    );
                }
                if status.slot <= to_slot && status.err.is_none() {
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
        records.sort_by(|left, right| {
            (left.slot, &left.signature).cmp(&(right.slot, &right.signature))
        });
        Ok((records, signatures_examined))
    })
    .await
    .context("Squads program history task panicked")?
}

async fn load_transactions(
    rpc: Arc<RpcClient>,
    history: Vec<HistoryRecord>,
    concurrency: usize,
) -> Result<Vec<EarnPolicyTransaction>> {
    let mut transactions = stream::iter(history)
        .map(|record| {
            let rpc = Arc::clone(&rpc);
            async move {
                match read_confirmed_squads_policy_transaction(rpc, record.signature, record.slot)
                    .await?
                {
                    EarnPolicyTransactionRead::NoStateChange => {
                        Ok::<Option<EarnPolicyTransaction>, anyhow::Error>(None)
                    }
                    EarnPolicyTransactionRead::Transaction(transaction) => Ok(Some(transaction)),
                }
            }
        })
        .buffer_unordered(concurrency)
        .try_filter_map(|transaction| async move { Ok(transaction) })
        .try_collect::<Vec<_>>()
        .await?;
    transactions
        .sort_by(|left, right| (left.slot, &left.signature).cmp(&(right.slot, &right.signature)));
    Ok(transactions)
}

fn history_record(status: &RpcConfirmedTransactionStatusWithSignature) -> HistoryRecord {
    HistoryRecord {
        signature: status.signature.clone(),
        slot: status.slot,
    }
}

fn write_report(path: &Path, report: &ReconciliationReport) -> Result<()> {
    let mut json = serde_json::to_vec_pretty(report)?;
    json.push(b'\n');
    fs::write(path, json).with_context(|| format!("write audit report {}", path.display()))
}

#[cfg(test)]
mod tests {
    use super::{policy_coverage, repair_signatures, CoverageStatus, PolicyFinding};

    #[test]
    fn absent_policy_is_missing_for_creation_but_complete_for_removal() {
        assert_eq!(policy_coverage(None, 100, false), CoverageStatus::Missing);
        assert_eq!(policy_coverage(None, 100, true), CoverageStatus::Completed);
    }

    #[test]
    fn stale_active_policy_requires_removal_replay() {
        assert_eq!(
            policy_coverage(Some((99, true)), 100, true),
            CoverageStatus::Missing
        );
        assert_eq!(
            policy_coverage(Some((100, false)), 100, true),
            CoverageStatus::Completed
        );
    }

    #[test]
    fn later_projection_covers_an_older_creation_event() {
        assert_eq!(
            policy_coverage(Some((101, true)), 100, false),
            CoverageStatus::Completed
        );
    }

    #[test]
    fn repair_replays_completed_removals_for_settings_with_missing_creations() {
        let finding = |kind: &str, signature: &str, coverage| PolicyFinding {
            kind: kind.to_owned(),
            signature: signature.to_owned(),
            slot: 100,
            settings: "settings".to_owned(),
            policy_account: None,
            vault_index: 1,
            coverage,
        };
        let signatures = repair_signatures(&[
            finding("yield_route", "create", CoverageStatus::Missing),
            finding("policy_removal", "remove", CoverageStatus::Completed),
        ]);

        assert_eq!(
            signatures.into_iter().collect::<Vec<_>>(),
            vec!["create".to_owned(), "remove".to_owned()]
        );
    }
}
