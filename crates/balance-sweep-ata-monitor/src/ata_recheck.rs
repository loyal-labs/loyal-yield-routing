//! Out-of-band reconciliation for wallet ATA updates the stream cannot record.
//!
//! A LaserStream update for a wallet ATA that is no longer an SPL Token account
//! carries the post-close state of a transaction the cluster may still roll
//! back, and the wallet may recreate the ATA moments later. Instead of trusting
//! that frame, the event loop hands the target to this queue, which waits and
//! then reads the account's actual state over RPC. Keeping the read here also
//! keeps RPC latency out of the hot event loop.
//!
//! The queue lives in memory, so a restart drops whatever is pending. The seed
//! path is the durable backstop: it records the same zero balance for every
//! target whose ATA is gone, so an abandoned recheck is repaired the next time
//! the session is rebuilt.

use std::{collections::HashMap, time::Duration};

use anyhow::{Context, Result};
use chrono::Utc;
use loyal_actions::USDC_MINT;
use loyal_observability::OperationalError;
use serde_json::json;
use solana_account_decoder::UiAccountEncoding;
use solana_client::{rpc_client::RpcClient, rpc_config::RpcAccountInfoConfig};
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::{
    sync::mpsc,
    task::JoinHandle,
    time::{self, Instant},
};

use crate::{
    account_data_hash, process_account_update, raw_account_data_base64, AtaObservationSink,
    AtaTarget, AtaUpdateOutcome, AtaUpdateSkip, BalanceSweepAtaObservation,
    ObservationInsertOutcome, CONFIRMED_COMMITMENT, RPC_SEED_SOURCE,
};

pub const RPC_RECHECK_SOURCE: &str = "rpc_recheck";

#[derive(Debug, Clone, Copy)]
pub struct AtaRecheckConfig {
    /// How long to wait after the skipped update before reading the account.
    pub delay: Duration,
    /// How long to wait before retrying a recheck that failed.
    pub retry_backoff: Duration,
    /// How many reads a single target gets before the recheck is abandoned.
    pub max_attempts: u32,
}

impl Default for AtaRecheckConfig {
    fn default() -> Self {
        Self {
            delay: Duration::from_secs(30),
            retry_backoff: Duration::from_secs(30),
            max_attempts: 3,
        }
    }
}

/// Why a target's routeable USDC balance is zero.
///
/// Carried into the observation's evidence so a zero row can always be traced
/// back to the on-chain state that produced it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ZeroBalanceCause {
    AccountMissing,
    NonSplTokenOwner { owner: Pubkey },
    UnexpectedMint { mint: Pubkey },
}

impl ZeroBalanceCause {
    fn as_str(self) -> &'static str {
        match self {
            Self::AccountMissing => "account_missing",
            Self::NonSplTokenOwner { .. } => "non_spl_token_owner",
            Self::UnexpectedMint { .. } => "unexpected_mint",
        }
    }
}

/// A failed recheck, split by what an operator would have to do about it.
///
/// Transport failures are the RPC node's problem and retry cleanly. Observation
/// failures — a sink write or a decode invariant — mean the balance never
/// landed, so abandoning one has to be visible rather than silent.
#[derive(Debug)]
enum RecheckError {
    Rpc(anyhow::Error),
    Observation(anyhow::Error),
}

impl RecheckError {
    fn kind(&self) -> &'static str {
        match self {
            Self::Rpc(_) => "rpc",
            Self::Observation(_) => "observation",
        }
    }

    fn recovery_required(&self) -> bool {
        matches!(self, Self::Observation(_))
    }

    fn error(&self) -> &anyhow::Error {
        match self {
            Self::Rpc(error) | Self::Observation(error) => error,
        }
    }
}

#[derive(Debug)]
struct AtaRecheckRequest {
    target: AtaTarget,
    skip: AtaUpdateSkip,
    stream_slot: u64,
}

#[derive(Debug)]
struct PendingRecheck {
    target: AtaTarget,
    skip: AtaUpdateSkip,
    stream_slot: u64,
    attempt: u32,
    due_at: Instant,
}

/// Enqueues wallet ATA rechecks. Cloning is cheap and keeps the worker alive.
#[derive(Clone, Debug)]
pub struct AtaRecheckHandle {
    tx: mpsc::UnboundedSender<AtaRecheckRequest>,
}

impl AtaRecheckHandle {
    pub fn enqueue(&self, target: &AtaTarget, skip: AtaUpdateSkip, stream_slot: u64) {
        let request = AtaRecheckRequest {
            target: target.clone(),
            skip,
            stream_slot,
        };
        if self.tx.send(request).is_err() {
            tracing::warn!(
                wallet_usdc_ata = %target.wallet_usdc_ata,
                "wallet ATA recheck worker is gone, dropping recheck"
            );
        }
    }
}

/// Spawns the recheck worker. The worker stops once every handle is dropped.
pub fn spawn_ata_recheck_worker<S>(
    rpc_url: String,
    sink: S,
    config: AtaRecheckConfig,
) -> (AtaRecheckHandle, JoinHandle<()>)
where
    S: AtaObservationSink + Send + Sync + 'static,
{
    let (tx, rx) = mpsc::unbounded_channel();
    let task = tokio::spawn(async move { run_recheck_worker(rx, rpc_url, sink, config).await });
    (AtaRecheckHandle { tx }, task)
}

/// Records the zero balance of a wallet ATA that RPC reports as gone.
///
/// The seed path already holds an authoritative read at a known slot, so it
/// records the zero directly instead of queueing another read.
pub async fn record_missing_ata_zero_balance(
    target: &AtaTarget,
    slot: u64,
    sink: &impl AtaObservationSink,
) -> Result<ObservationInsertOutcome> {
    record_zero_balance(
        ZeroBalanceRecord {
            target,
            slot,
            cause: ZeroBalanceCause::AccountMissing,
            data: &[],
            source: RPC_SEED_SOURCE,
            stream_slot: None,
            stream_reason: None,
        },
        sink,
    )
    .await
}

async fn run_recheck_worker<S>(
    mut rx: mpsc::UnboundedReceiver<AtaRecheckRequest>,
    rpc_url: String,
    sink: S,
    config: AtaRecheckConfig,
) where
    S: AtaObservationSink + Send + Sync + 'static,
{
    let mut pending: HashMap<Pubkey, PendingRecheck> = HashMap::new();
    loop {
        let next_due = pending.values().map(|entry| entry.due_at).min();
        tokio::select! {
            request = rx.recv() => {
                let Some(request) = request else {
                    break;
                };
                // A later update for the same ATA replaces the pending one: only
                // the account's final state matters, and one read settles it.
                pending.insert(
                    request.target.wallet_usdc_ata,
                    PendingRecheck {
                        target: request.target,
                        skip: request.skip,
                        stream_slot: request.stream_slot,
                        attempt: 0,
                        due_at: Instant::now() + config.delay,
                    },
                );
            }
            _ = wait_until(next_due) => {
                run_due_rechecks(&mut pending, &rpc_url, &sink, config).await;
            }
        }
    }
}

async fn wait_until(deadline: Option<Instant>) {
    match deadline {
        Some(deadline) => time::sleep_until(deadline).await,
        None => std::future::pending::<()>().await,
    }
}

async fn run_due_rechecks<S>(
    pending: &mut HashMap<Pubkey, PendingRecheck>,
    rpc_url: &str,
    sink: &S,
    config: AtaRecheckConfig,
) where
    S: AtaObservationSink,
{
    let now = Instant::now();
    let due = pending
        .iter()
        .filter(|(_, entry)| entry.due_at <= now)
        .map(|(account, _)| *account)
        .collect::<Vec<_>>();
    for account in due {
        let Some(mut entry) = pending.remove(&account) else {
            continue;
        };
        entry.attempt = entry.attempt.saturating_add(1);
        let Err(error) = recheck_wallet_ata(&entry, rpc_url, sink).await else {
            continue;
        };
        if entry.attempt < config.max_attempts {
            tracing::warn!(
                wallet_usdc_ata = %account,
                attempt = entry.attempt,
                kind = error.kind(),
                error = %error.error(),
                "retrying wallet ATA recheck"
            );
            entry.due_at = Instant::now() + config.retry_backoff;
            pending.insert(account, entry);
            continue;
        }
        // The balance the pipeline can act on is now unverified, and for a sink
        // failure the observation never landed at all. Say so out loud instead
        // of leaving a stale balance behind a healthy-looking monitor.
        tracing::warn!(
            wallet_usdc_ata = %account,
            attempts = entry.attempt,
            kind = error.kind(),
            error = %error.error(),
            "abandoning wallet ATA recheck"
        );
        OperationalError::new(
            "balance_sweep_ata_recheck_abandoned",
            "recheck_wallet_ata",
            "Balance sweep ATA recheck abandoned after repeated failures",
        )
        .retryable(true)
        .recovery_required(error.recovery_required())
        .emit();
    }
}

async fn recheck_wallet_ata<S>(
    entry: &PendingRecheck,
    rpc_url: &str,
    sink: &S,
) -> Result<(), RecheckError>
where
    S: AtaObservationSink,
{
    let account = entry.target.wallet_usdc_ata;
    let fetched = fetch_account(rpc_url, account, entry.stream_slot)
        .await
        .map_err(RecheckError::Rpc)?;
    let slot = fetched.context.slot;

    let Some(fetched_account) = fetched.value else {
        record_zero_balance(
            ZeroBalanceRecord {
                target: &entry.target,
                slot,
                cause: ZeroBalanceCause::AccountMissing,
                data: &[],
                source: RPC_RECHECK_SOURCE,
                stream_slot: Some(entry.stream_slot),
                stream_reason: Some(entry.skip.reason()),
            },
            sink,
        )
        .await
        .map_err(RecheckError::Observation)?;
        return Ok(());
    };

    let data = fetched_account.data.clone();
    let outcome = process_account_update(
        &entry.target,
        fetched_account.lamports,
        slot,
        fetched_account.owner,
        fetched_account.data,
        RPC_RECHECK_SOURCE,
        CONFIRMED_COMMITMENT,
        None,
        Utc::now(),
        sink,
    )
    .await
    .map_err(RecheckError::Observation)?;

    // Either skip means the account holds no routeable USDC, so both settle as
    // an evidence-backed zero rather than an unreconciled warning.
    let cause = match outcome {
        AtaUpdateOutcome::Recorded(recorded) => {
            tracing::info!(
                wallet_usdc_ata = %account,
                slot,
                event_id = recorded.event_id,
                inserted = recorded.inserted,
                "recorded wallet ATA recheck observation"
            );
            return Ok(());
        }
        AtaUpdateOutcome::Skipped(AtaUpdateSkip::NonSplTokenOwner { owner }) => {
            ZeroBalanceCause::NonSplTokenOwner { owner }
        }
        AtaUpdateOutcome::Skipped(AtaUpdateSkip::UnexpectedMint { mint }) => {
            ZeroBalanceCause::UnexpectedMint { mint }
        }
    };
    record_zero_balance(
        ZeroBalanceRecord {
            target: &entry.target,
            slot,
            cause,
            data: &data,
            source: RPC_RECHECK_SOURCE,
            stream_slot: Some(entry.stream_slot),
            stream_reason: Some(entry.skip.reason()),
        },
        sink,
    )
    .await
    .map_err(RecheckError::Observation)?;
    Ok(())
}

async fn fetch_account(
    rpc_url: &str,
    account: Pubkey,
    min_context_slot: u64,
) -> Result<solana_client::rpc_response::Response<Option<solana_sdk::account::Account>>> {
    let rpc_url = rpc_url.to_owned();
    // A node lagging behind the frame that triggered the recheck would answer
    // with pre-close state, and the slot-monotonic balance row would drop the
    // zero on the floor while the recheck counted as done. Make the node prove
    // it is caught up, and let the retry path handle it when it is not.
    let config = RpcAccountInfoConfig {
        encoding: Some(UiAccountEncoding::Base64),
        commitment: Some(CommitmentConfig::confirmed()),
        data_slice: None,
        min_context_slot: Some(min_context_slot),
    };
    let fetched = tokio::task::spawn_blocking(move || {
        let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::confirmed());
        rpc.get_account_with_config(&account, config)
    })
    .await
    .context("join wallet ATA recheck task")?
    .with_context(|| format!("fetch wallet ATA {account} recheck state"))?;
    if fetched.context.slot < min_context_slot {
        anyhow::bail!(
            "wallet ATA {account} recheck answered at slot {} before {min_context_slot}",
            fetched.context.slot
        );
    }
    Ok(fetched)
}

struct ZeroBalanceRecord<'a> {
    target: &'a AtaTarget,
    slot: u64,
    cause: ZeroBalanceCause,
    data: &'a [u8],
    source: &'static str,
    stream_slot: Option<u64>,
    stream_reason: Option<&'static str>,
}

async fn record_zero_balance(
    record: ZeroBalanceRecord<'_>,
    sink: &impl AtaObservationSink,
) -> Result<ObservationInsertOutcome> {
    let ZeroBalanceRecord {
        target,
        slot,
        cause,
        data,
        source,
        stream_slot,
        stream_reason,
    } = record;
    let observed_at = Utc::now();
    let hash = account_data_hash(data);
    let encoded = raw_account_data_base64(data);
    let observation = BalanceSweepAtaObservation {
        target_id: target.id,
        cluster: target.cluster.clone(),
        wallet: target.wallet.clone(),
        wallet_usdc_ata: target.wallet_usdc_ata.to_string(),
        vault_pubkey: target.vault_pubkey.clone(),
        vault_usdc_ata: target.vault_usdc_ata.to_string(),
        amount_raw: 0,
        owner: Some(target.wallet.clone()),
        mint: USDC_MINT.to_string(),
        slot,
        observed_at,
        source: source.to_owned(),
        source_commitment: CONFIRMED_COMMITMENT.to_owned(),
        txn_signature: None,
        account_data_hash: hash.clone(),
        raw_account_data_base64: encoded.clone(),
        raw_evidence: json!({
            "zero_balance_cause": cause.as_str(),
            "observed_owner": observed_owner(cause),
            "observed_mint": observed_mint(cause),
            "account_data_hash": hash,
            "raw_account_data_base64": encoded,
            "source": source,
            "stream_slot": stream_slot,
            "stream_skip_reason": stream_reason,
            "wallet": target.wallet,
            "wallet_usdc_ata": target.wallet_usdc_ata.to_string(),
            "vault_pubkey": target.vault_pubkey,
            "vault_usdc_ata": target.vault_usdc_ata.to_string(),
        }),
        received_at: observed_at,
    };
    let outcome = sink.record_observation(observation).await?;
    tracing::info!(
        wallet_usdc_ata = %target.wallet_usdc_ata,
        slot,
        event_id = outcome.event_id,
        inserted = outcome.inserted,
        cause = cause.as_str(),
        source,
        "recorded zero wallet ATA balance"
    );
    Ok(outcome)
}

fn observed_owner(cause: ZeroBalanceCause) -> Option<String> {
    match cause {
        ZeroBalanceCause::NonSplTokenOwner { owner } => Some(owner.to_string()),
        _ => None,
    }
}

fn observed_mint(cause: ZeroBalanceCause) -> Option<String> {
    match cause {
        ZeroBalanceCause::UnexpectedMint { mint } => Some(mint.to_string()),
        _ => None,
    }
}
